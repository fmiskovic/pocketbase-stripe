package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/jsvm"

	"github.com/stripe/stripe-go/v76"
	"github.com/stripe/stripe-go/v76/billingportal/session"
	checkoutSession "github.com/stripe/stripe-go/v76/checkout/session"
	"github.com/stripe/stripe-go/v76/customer"
	"github.com/stripe/stripe-go/v76/webhook"
)

var (
	stripeSuccessURL       string
	stripeCancelURL        string
	stripeBillingReturnURL string
	WHSEC                  string
)

func init() {
	stripeSuccessURL = os.Getenv("STRIPE_SUCCESS_URL")
	stripeCancelURL = os.Getenv("STRIPE_CANCEL_URL")
	stripeBillingReturnURL = os.Getenv("STRIPE_BILLING_RETURN_URL")
	WHSEC = os.Getenv("STRIPE_WHSEC")
}

func coalesce(value *string, defaultValue string) string {
	if value != nil {
		return *value
	}
	return defaultValue
}

func int64ToISODate(timestamp int64) string {
	// convert unix timestamp to a time.Time
	t := time.Unix(timestamp, 0)

	// format the time as an ISO 8601 date string (in UTC)
	return t.Format(time.RFC3339)
}

func main() {
	app := pocketbase.New()

	// retrieve your STRIPE_SECRET_KEY from environment variables
	stripe.Key = os.Getenv("STRIPE_SECRET_KEY")

	// register JSVM plugin for JavaScript hooks
	jsvm.MustRegister(app, jsvm.Config{
		HooksWatch:    true,
		HooksPoolSize: 25,
	})

	// register all routes
	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		se.Router.GET("/goext/{name}", handleHello)
		se.Router.POST("/create-checkout-session", handleCreateCheckoutSession)
		se.Router.POST("/create-portal-link", handleCreatePortalLink)
		se.Router.POST("/stripe", handleStripeWebhook)

		return se.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func handleHello(e *core.RequestEvent) error {
	name := e.Request.PathValue("name")
	return e.JSON(http.StatusOK, map[string]string{"message": "Hello " + name})
}

func handleCreateCheckoutSession(e *core.RequestEvent) error {
	// 1. destructure the price and quantity from the POST body
	payload, err := io.ReadAll(e.Request.Body)
	if err != nil {
		e.App.Logger().Error("could not read request body", "error", err)
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not read request body"})
	}
	var data map[string]interface{}
	if err = json.Unmarshal(payload, &data); err != nil {
		e.App.Logger().Error("could not parse request body", "error", err)
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not parse request body"})
	}

	price, ok := data["price"].(map[string]interface{})
	if !ok || price == nil {
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "invalid price data"})
	}
	quantity, ok := data["quantity"].(float64)
	if !ok {
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "invalid quantity"})
	}
	priceType, ok := price["type"].(string)
	if !ok || priceType == "" {
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "invalid price type"})
	}
	priceID, ok := price["id"].(string)
	if !ok || priceID == "" {
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "invalid price id"})
	}

	// 2. get the user from pocketbase auth
	token := e.Request.Header.Get("Authorization")
	record, err := e.App.FindAuthRecordByToken(token, core.TokenTypeAuth)
	if err != nil {
		e.App.Logger().Error("could not find auth record by token", "error", err)
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not find auth record by token"})
	}

	// 3. retrieve or create the customer in Stripe
	existingCustomerRecord, err := e.App.FindFirstRecordByData("customer", "user_id", record.Id)
	if err != nil {
		// create new customer if none exists
		customerEmail := record.GetString("email")
		customerParams := &stripe.CustomerParams{
			Email: &customerEmail,
			Metadata: map[string]string{
				"pocketbaseUUID": record.GetString("id"),
			},
		}

		stripeCustomer, err := customer.New(customerParams)
		if err != nil {
			e.App.Logger().Error("could not create customer", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create Stripe customer"})
		}

		// upload customer to pocketbase
		collection, err := e.App.FindCollectionByNameOrId("customer")
		if err != nil {
			e.App.Logger().Error("could not find collection customer", "error", err)
			return e.JSON(http.StatusInternalServerError, map[string]string{"failure": "could not find collection customer"})
		}

		newCustomerRecord := core.NewRecord(collection)
		newCustomerRecord.Set("user_id", record.Id)
		newCustomerRecord.Set("stripe_customer_id", stripeCustomer.ID)

		if err = e.App.Save(newCustomerRecord); err != nil {
			e.App.Logger().Error("could not save new customer record", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create new customer"})
		}

		// do pricing new customer
		if priceType == "recurring" {
			lineParams := []*stripe.CheckoutSessionLineItemParams{
				{
					Price:    stripe.String(priceID),
					Quantity: stripe.Int64(int64(quantity)),
				},
			}
			customerUpdateParams := &stripe.CheckoutSessionCustomerUpdateParams{
				Address: stripe.String("auto"),
			}
			subscriptionParams := &stripe.CheckoutSessionSubscriptionDataParams{
				Metadata: map[string]string{},
			}

			sessionParams := &stripe.CheckoutSessionParams{
				Customer:                 &stripeCustomer.ID,
				PaymentMethodTypes:       stripe.StringSlice([]string{"card"}),
				BillingAddressCollection: stripe.String("required"),
				CustomerUpdate:           customerUpdateParams,
				Mode:                     stripe.String("subscription"),
				AllowPromotionCodes:      stripe.Bool(true),
				SuccessURL:               &stripeSuccessURL,
				CancelURL:                &stripeCancelURL,
				LineItems:                lineParams,
				SubscriptionData:         subscriptionParams,
			}
			sesh, err := checkoutSession.New(sessionParams)
			if err != nil {
				e.App.Logger().Error("could not create checkout session", "error", err)
				return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create new session"})
			}
			return e.JSON(http.StatusOK, sesh)
		} else if priceType == "one_time" {
			lineParams := []*stripe.CheckoutSessionLineItemParams{
				{
					Price:    stripe.String(priceID),
					Quantity: stripe.Int64(int64(quantity)),
				},
			}
			customerUpdateParams := &stripe.CheckoutSessionCustomerUpdateParams{
				Address: stripe.String("auto"),
			}

			sessionParams := &stripe.CheckoutSessionParams{
				Customer:                 &stripeCustomer.ID,
				PaymentMethodTypes:       stripe.StringSlice([]string{"card"}),
				BillingAddressCollection: stripe.String("required"),
				CustomerUpdate:           customerUpdateParams,
				Mode:                     stripe.String("payment"),
				AllowPromotionCodes:      stripe.Bool(true),
				SuccessURL:               &stripeSuccessURL,
				CancelURL:                &stripeCancelURL,
				LineItems:                lineParams,
			}
			sesh, err := checkoutSession.New(sessionParams)
			if err != nil {
				e.App.Logger().Error("could not create checkout session", "error", err)
				return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create new session"})
			}
			return e.JSON(http.StatusOK, sesh)
		}

		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create new session"})
	}
	// Do Pricing Existing Customer
	if priceType == "recurring" {
		lineParams := []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceID),
				Quantity: stripe.Int64(int64(quantity)),
			},
		}
		customerUpdateParams := &stripe.CheckoutSessionCustomerUpdateParams{
			Address: stripe.String("auto"),
		}
		subscriptionParams := &stripe.CheckoutSessionSubscriptionDataParams{
			Metadata: map[string]string{},
		}

		sessionParams := &stripe.CheckoutSessionParams{
			Customer:                 stripe.String(existingCustomerRecord.GetString("stripe_customer_id")),
			PaymentMethodTypes:       stripe.StringSlice([]string{"card"}),
			BillingAddressCollection: stripe.String("required"),
			CustomerUpdate:           customerUpdateParams,
			Mode:                     stripe.String("subscription"),
			AllowPromotionCodes:      stripe.Bool(true),
			SuccessURL:               &stripeSuccessURL,
			CancelURL:                &stripeCancelURL,
			LineItems:                lineParams,
			SubscriptionData:         subscriptionParams,
		}
		sesh, err := checkoutSession.New(sessionParams)
		if err != nil {
			e.App.Logger().Error("could not create checkout session", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create new session"})
		}
		return e.JSON(http.StatusOK, sesh)
	} else if priceType == "one_time" {
		lineParams := []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceID),
				Quantity: stripe.Int64(int64(quantity)),
			},
		}
		customerUpdateParams := &stripe.CheckoutSessionCustomerUpdateParams{
			Address: stripe.String("auto"),
		}

		sessionParams := &stripe.CheckoutSessionParams{
			Customer:                 stripe.String(existingCustomerRecord.GetString("stripe_customer_id")),
			PaymentMethodTypes:       stripe.StringSlice([]string{"card"}),
			BillingAddressCollection: stripe.String("required"),
			CustomerUpdate:           customerUpdateParams,
			Mode:                     stripe.String("payment"),
			AllowPromotionCodes:      stripe.Bool(true),
			SuccessURL:               &stripeSuccessURL,
			CancelURL:                &stripeCancelURL,
			LineItems:                lineParams,
		}
		sesh, err := checkoutSession.New(sessionParams)
		if err != nil {
			e.App.Logger().Error("could not create checkout session", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create new session"})
		}
		return e.JSON(http.StatusOK, sesh)
	}

	return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create new session for stripe"})
}

func handleCreatePortalLink(e *core.RequestEvent) error {
	// 1. get the user from pocketbase auth
	token := e.Request.Header.Get("Authorization")
	record, err := e.App.FindAuthRecordByToken(token, core.TokenTypeAuth)
	if err != nil {
		e.App.Logger().Error("could not find auth record by token", "error", err)
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not find auth record by token"})
	}

	// 2. retrieve or create the customer in Stripe
	existingCustomerRecord, err := e.App.FindFirstRecordByData("customer", "user_id", record.Id)
	if err != nil {
		// create new customer if none exists
		customerParams := &stripe.CustomerParams{
			Metadata: map[string]string{
				"pocketbaseUUID": record.GetString("id"),
			},
		}

		stripeCustomer, err := customer.New(customerParams)
		if err != nil {
			e.App.Logger().Error("could not create customer", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create Stripe customer"})
		}

		// upload customer to pocketbase
		collection, err := e.App.FindCollectionByNameOrId("customer")
		if err != nil {
			e.App.Logger().Error("could not find collection customer", "error", err)
			return e.JSON(http.StatusInternalServerError, map[string]string{"failure": "could not find collection customer"})
		}

		newCustomerRecord := core.NewRecord(collection)
		newCustomerRecord.Set("user_id", record.Id)
		newCustomerRecord.Set("stripe_customer_id", stripeCustomer.ID)

		if err = e.App.Save(newCustomerRecord); err != nil {
			e.App.Logger().Error("could not save new customer record", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create new customer"})
		}

		// create new session
		sessionParams := &stripe.BillingPortalSessionParams{
			Customer:  stripe.String(stripeCustomer.ID),
			ReturnURL: &stripeBillingReturnURL,
		}
		sesh, err := session.New(sessionParams)
		if err != nil {
			e.App.Logger().Error("could not create billing portal session", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create new session"})
		}
		return e.JSON(http.StatusOK, sesh)
	}

	// create new session for existing customer
	sessionParams := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(existingCustomerRecord.GetString("stripe_customer_id")),
		ReturnURL: &stripeBillingReturnURL,
	}
	sesh, err := session.New(sessionParams)
	if err != nil {
		e.App.Logger().Error("could not create billing portal session", "error", err)
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not create new session"})
	}
	return e.JSON(http.StatusOK, sesh)
}

func handleStripeWebhook(e *core.RequestEvent) error {
	// read the request body into a byte slice
	payload, err := io.ReadAll(e.Request.Body)
	if err != nil {
		e.App.Logger().Error("failed to read request body", "error", err)
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "failed to read request body"})
	}

	signatureHeader := e.Request.Header.Get("Stripe-Signature")
	event, err := webhook.ConstructEvent(payload, signatureHeader, WHSEC)
	if err != nil {
		e.App.Logger().Error("webhook verification failed", "error", err)
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "webhook verification failed"})
	}

	switch event.Type {
	case "product.created", "product.updated":
		var product stripe.Product
		err = json.Unmarshal(event.Data.Raw, &product)
		if err != nil {
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "failed to marshall the stripe event"})
		}

		collection, err := e.App.FindCollectionByNameOrId("product")
		if err != nil {
			e.App.Logger().Error("Could not find collection product", "error", err)
			return e.JSON(http.StatusInternalServerError, map[string]string{"failure": "could not find collection product"})
		}

		existingRecord, err := e.App.FindFirstRecordByData("product", "product_id", product.ID)
		var recordToSave *core.Record

		if err == nil && existingRecord != nil {
			// existing record found, update it
			recordToSave = existingRecord
		} else {
			// existing record not found, insert a new record
			recordToSave = core.NewRecord(collection)
		}

		recordToSave.Set("product_id", product.ID)
		recordToSave.Set("active", product.Active)
		recordToSave.Set("name", product.Name)
		recordToSave.Set("description", coalesce(&product.Description, ""))
		recordToSave.Set("metadata", product.Metadata)

		if err = e.App.Save(recordToSave); err != nil {
			e.App.Logger().Error("Could not save product record", "error", err)
			return err
		}

	case "price.created", "price.updated":
		var price stripe.Price
		err = json.Unmarshal(event.Data.Raw, &price)
		if err != nil {
			e.App.Logger().Error("failed to unmarshall the stripe price event", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "failed to marshall the stripe event"})
		}

		collection, err := e.App.FindCollectionByNameOrId("price")
		if err != nil {
			e.App.Logger().Error("Could not find collection price", "error", err)
			return e.JSON(http.StatusInternalServerError, map[string]string{"failure": "could not find collection price"})
		}

		existingRecord, err := e.App.FindFirstRecordByData("price", "price_id", price.ID)
		var recordToSave *core.Record

		if err == nil && existingRecord != nil {
			// existing record found, update it
			recordToSave = existingRecord
		} else {
			// existing record not found, insert a new record
			recordToSave = core.NewRecord(collection)
		}

		recordToSave.Set("price_id", price.ID)
		recordToSave.Set("product_id", price.Product.ID)
		recordToSave.Set("active", price.Active)
		recordToSave.Set("currency", price.Currency)
		recordToSave.Set("description", price.Nickname)
		recordToSave.Set("type", price.Type)
		recordToSave.Set("unit_amount", price.UnitAmount)
		recordToSave.Set("metadata", price.Metadata)

		// check if recurring is not nil before accessing its fields
		if price.Recurring != nil {
			recordToSave.Set("interval", price.Recurring.Interval)
			recordToSave.Set("interval_count", price.Recurring.IntervalCount)
			recordToSave.Set("trial_period_days", price.Recurring.TrialPeriodDays)
		}

		if err = e.App.Save(recordToSave); err != nil {
			e.App.Logger().Error("could not save price record", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "could not save price record"})
		}

	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		var subscription stripe.Subscription
		err = json.Unmarshal(event.Data.Raw, &subscription)
		if err != nil {
			e.App.Logger().Error("failed to unmarshall the stripe subscription event", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "failed to marshall the stripe event"})
		}

		// get customer's UUID from mapping table
		if subscription.Customer == nil {
			e.App.Logger().Error("subscription missing customer")
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "missing subscription customer"})
		}
		if len(subscription.Items.Data) == 0 || subscription.Items.Data[0].Price == nil {
			e.App.Logger().Error("subscription has no items or price is nil")
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "subscription has no items"})
		}
		item := subscription.Items.Data[0]

		existingCustomer, err := e.App.FindFirstRecordByData("customer", "stripe_customer_id", subscription.Customer.ID)
		if err != nil {
			e.App.Logger().Error("could not find customer record for subscription", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "no customer"})
		}

		uuid := existingCustomer.GetString("user_id")
		collection, err := e.App.FindCollectionByNameOrId("subscription")
		if err != nil {
			e.App.Logger().Error("could not find collection subscription", "error", err)
			return e.JSON(http.StatusInternalServerError, map[string]string{"failure": "collection doesn't exist"})
		}

		// update Subscription Details
		existingRecord, err := e.App.FindFirstRecordByData("subscription", "subscription_id", subscription.ID)
		var recordToSave *core.Record

		if err == nil && existingRecord != nil {
			recordToSave = existingRecord
		} else {
			recordToSave = core.NewRecord(collection)
		}

		recordToSave.Set("subscription_id", subscription.ID)
		recordToSave.Set("user_id", uuid)
		recordToSave.Set("metadata", subscription.Metadata)
		recordToSave.Set("status", subscription.Status)
		recordToSave.Set("price_id", item.Price.ID)
		recordToSave.Set("quantity", item.Quantity)
		recordToSave.Set("cancel_at_period_end", subscription.CancelAtPeriodEnd)
		recordToSave.Set("cancel_at", int64ToISODate(subscription.CancelAt))
		recordToSave.Set("canceled_at", int64ToISODate(subscription.CanceledAt))
		recordToSave.Set("current_period_start", int64ToISODate(subscription.CurrentPeriodStart))
		recordToSave.Set("current_period_end", int64ToISODate(subscription.CurrentPeriodEnd))
		recordToSave.Set("created", int64ToISODate(item.Created))
		recordToSave.Set("ended_at", int64ToISODate(subscription.EndedAt))
		recordToSave.Set("trial_start", int64ToISODate(subscription.TrialStart))
		recordToSave.Set("trial_end", int64ToISODate(subscription.TrialEnd))

		if err = e.App.Save(recordToSave); err != nil {
			e.App.Logger().Error("could not save subscription record", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "couldn't submit subscription update"})
		}

		// Update User Details If Subscription Created
		if event.Type == "customer.subscription.created" {
			existingUserRecord, err := e.App.FindFirstRecordByData("user", "id", uuid)
			if err == nil && existingUserRecord != nil && subscription.DefaultPaymentMethod != nil {
				if subscription.DefaultPaymentMethod.Customer != nil {
					existingUserRecord.Set("billing_address", subscription.DefaultPaymentMethod.Customer.Address)
				}
				existingUserRecord.Set("payment_method", subscription.DefaultPaymentMethod.Type)

				if err := e.App.Save(existingUserRecord); err != nil {
					e.App.Logger().Error("could not save user record", "userId", uuid, "error", err)
					return e.JSON(http.StatusBadRequest, map[string]string{"failure": "couldn't submit user update"})
				}
			}
		}

	case "checkout.session.completed":
		var checkoutSesh stripe.CheckoutSession
		err = json.Unmarshal(event.Data.Raw, &checkoutSesh)
		if err != nil {
			e.App.Logger().Error("failed to unmarshall the stripe checkout session event", "error", err)
			return e.JSON(http.StatusBadRequest, map[string]string{"failure": "failed to marshall the stripe event"})
		}

		if checkoutSesh.Mode == "subscription" {
			if checkoutSesh.Subscription == nil {
				e.App.Logger().Error("could not find checkout session subscription")
				return e.JSON(http.StatusBadRequest, map[string]string{"failure": "missing checkout subscription"})
			}
			if checkoutSesh.Subscription.Customer == nil {
				e.App.Logger().Error("could not find checkout session subscription customer")
				return e.JSON(http.StatusBadRequest, map[string]string{"failure": "missing checkout customer"})
			}
			if len(checkoutSesh.Subscription.Items.Data) == 0 || checkoutSesh.Subscription.Items.Data[0].Price == nil {
				e.App.Logger().Error("could not find checkout session subscription items")
				return e.JSON(http.StatusBadRequest, map[string]string{"failure": "subscription has no items"})
			}
			item := checkoutSesh.Subscription.Items.Data[0]

			// get customer's UUID from mapping table
			existingCustomer, err := e.App.FindFirstRecordByData("customer", "stripe_customer_id", checkoutSesh.Subscription.Customer.ID)
			if err != nil {
				e.App.Logger().Error("could not find customer record for checkout session subscription", "error", err)
				return e.JSON(http.StatusBadRequest, map[string]string{"failure": "no customer"})
			}

			uuid := existingCustomer.GetString("user_id")
			collection, err := e.App.FindCollectionByNameOrId("subscription")
			if err != nil {
				e.App.Logger().Error("could not find collection subscription", "error", err)
				return e.JSON(http.StatusInternalServerError, map[string]string{"failure": "collection doesn't exist"})
			}

			// update subscription details
			existingRecord, err := e.App.FindFirstRecordByData("subscription", "subscription_id", checkoutSesh.Subscription.ID)
			var recordToSave *core.Record

			if err == nil && existingRecord != nil {
				recordToSave = existingRecord
			} else {
				recordToSave = core.NewRecord(collection)
			}

			recordToSave.Set("subscription_id", checkoutSesh.Subscription.ID)
			recordToSave.Set("user_id", uuid)
			recordToSave.Set("metadata", checkoutSesh.Subscription.Metadata)
			recordToSave.Set("status", checkoutSesh.Subscription.Status)
			recordToSave.Set("price_id", item.Price.ID)
			recordToSave.Set("quantity", item.Quantity)
			recordToSave.Set("cancel_at_period_end", checkoutSesh.Subscription.CancelAtPeriodEnd)
			recordToSave.Set("cancel_at", int64ToISODate(checkoutSesh.Subscription.CancelAt))
			recordToSave.Set("canceled_at", int64ToISODate(checkoutSesh.Subscription.CanceledAt))
			recordToSave.Set("current_period_start", int64ToISODate(checkoutSesh.Subscription.CurrentPeriodStart))
			recordToSave.Set("current_period_end", int64ToISODate(checkoutSesh.Subscription.CurrentPeriodEnd))
			recordToSave.Set("created", int64ToISODate(item.Created))
			recordToSave.Set("ended_at", int64ToISODate(checkoutSesh.Subscription.EndedAt))
			recordToSave.Set("trial_start", int64ToISODate(checkoutSesh.Subscription.TrialStart))
			recordToSave.Set("trial_end", int64ToISODate(checkoutSesh.Subscription.TrialEnd))

			if err = e.App.Save(recordToSave); err != nil {
				e.App.Logger().Error("could not save subscription record", "error", err)
				return e.JSON(http.StatusBadRequest, map[string]string{"failure": "couldn't submit subscription update"})
			}

			// update user details
			existingUserRecord, err := e.App.FindFirstRecordByData("user", "id", uuid)
			if err == nil && existingUserRecord != nil && checkoutSesh.Subscription.DefaultPaymentMethod != nil {
				if checkoutSesh.Subscription.DefaultPaymentMethod.Customer != nil {
					existingUserRecord.Set("billing_address", checkoutSesh.Subscription.DefaultPaymentMethod.Customer.Address)
				}
				existingUserRecord.Set("payment_method", checkoutSesh.Subscription.DefaultPaymentMethod.Type)

				if err = e.App.Save(existingUserRecord); err != nil {
					e.App.Logger().Error("could not save user record after checkout session completion", "error", err)
					return e.JSON(http.StatusBadRequest, map[string]string{"failure": "couldn't submit user update"})
				}
			}
		}

	default:
		return e.JSON(http.StatusBadRequest, map[string]string{"failure": "didn't receive a valid event"})
	}

	return e.JSON(http.StatusOK, map[string]interface{}{"success": "data was received"})
}
