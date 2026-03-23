package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/stripe/stripe-go/v76"
	"github.com/stripe/stripe-go/v76/webhook"
)

type endpointScenario struct {
	name            string
	method          string
	url             string
	body            string
	expectedStatus  int
	expectedContent []string
	headers         map[string]string
	setup           func(t testing.TB, app *tests.TestApp, scenario *tests.ApiScenario)
	after           func(t testing.TB, app *tests.TestApp, res *http.Response)
}

func registerRoutes(e *core.ServeEvent) {
	e.Router.GET("/goext/{name}", handleHello)
	e.Router.POST("/create-checkout-session", handleCreateCheckoutSession)
	e.Router.POST("/create-portal-link", handleCreatePortalLink)
	e.Router.POST("/stripe", handleStripeWebhook)
}

func runEndpointScenarios(t *testing.T, cases []endpointScenario) {
	t.Helper()

	for _, tc := range cases {
		tc := tc
		scenario := tests.ApiScenario{
			Name:            tc.name,
			Method:          tc.method,
			URL:             tc.url,
			ExpectedStatus:  tc.expectedStatus,
			ExpectedContent: tc.expectedContent,
		}
		if tc.body != "" {
			scenario.Body = strings.NewReader(tc.body)
		}
		if len(tc.headers) > 0 {
			scenario.Headers = tc.headers
		}
		scenario.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
			registerRoutes(e)
			if tc.setup != nil {
				tc.setup(t, app, &scenario)
			}
		}
		scenario.AfterTestFunc = func(t testing.TB, app *tests.TestApp, res *http.Response) {
			if tc.after != nil {
				tc.after(t, app, res)
			}
		}
		scenario.Test(t)
	}
}

func setupStripeMock(t testing.TB) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/customers":
			writeStripeResponse(w, `{"id":"cus_test","object":"customer"}`)
		case "/v1/checkout/sessions":
			writeStripeResponse(w, `{"id":"cs_test","object":"checkout.session"}`)
		case "/v1/billing_portal/sessions":
			writeStripeResponse(w, `{"id":"bps_test","object":"billing_portal.session","url":"https://example.com/portal"}`)
		default:
			http.NotFound(w, r)
		}
	}))

	originalBackend := stripe.GetBackend(stripe.APIBackend)
	backend := stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:           stripe.String(server.URL),
		HTTPClient:    server.Client(),
		LeveledLogger: stripe.DefaultLeveledLogger,
	})
	stripe.SetBackend(stripe.APIBackend, backend)
	stripe.Key = "sk_test"

	t.Cleanup(func() {
		stripe.SetBackend(stripe.APIBackend, originalBackend)
		server.Close()
	})
}

func writeStripeResponse(w http.ResponseWriter, body string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func ensureCustomerCollection(t testing.TB, app *tests.TestApp) *core.Collection {
	t.Helper()

	collection, err := app.FindCollectionByNameOrId("customer")
	if err == nil && collection != nil {
		return collection
	}

	collection = core.NewBaseCollection("customer")
	collection.Fields.Add(
		&core.TextField{Name: "user_id", Required: true},
		&core.TextField{Name: "stripe_customer_id", Required: true},
	)

	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	return collection
}

func ensureProductCollection(t testing.TB, app *tests.TestApp) *core.Collection {
	t.Helper()

	collection, err := app.FindCollectionByNameOrId("product")
	if err == nil && collection != nil {
		return collection
	}

	collection = core.NewBaseCollection("product")
	collection.Fields.Add(
		&core.TextField{Name: "product_id", Required: true},
		&core.BoolField{Name: "active"},
		&core.TextField{Name: "name"},
		&core.TextField{Name: "description"},
		&core.JSONField{Name: "metadata"},
	)

	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	return collection
}

func authTokenForTestUser(t testing.TB, app *tests.TestApp) (*core.Record, string) {
	t.Helper()

	user, err := app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	token, err := user.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	return user, token
}

func TestHelloEndpoint(t *testing.T) {
	runEndpointScenarios(t, []endpointScenario{
		{
			name:           "hello endpoint",
			method:         http.MethodGet,
			url:            "/goext/Stripe",
			expectedStatus: http.StatusOK,
			expectedContent: []string{
				`"message":"Hello Stripe"`,
			},
		},
		{
			name:           "hello endpoint different name",
			method:         http.MethodGet,
			url:            "/goext/World",
			expectedStatus: http.StatusOK,
			expectedContent: []string{
				`"message":"Hello World"`,
			},
		},
	})
}

func TestCreateCheckoutSessionEndpoint(t *testing.T) {
	runEndpointScenarios(t, []endpointScenario{
		{
			name:           "checkout session invalid json",
			method:         http.MethodPost,
			url:            "/create-checkout-session",
			body:           `{"price":`,
			expectedStatus: http.StatusBadRequest,
			expectedContent: []string{
				`"failure":"could not parse request body"`,
			},
		},
		{
			name:           "checkout session requires price",
			method:         http.MethodPost,
			url:            "/create-checkout-session",
			body:           `{"quantity":1}`,
			expectedStatus: http.StatusBadRequest,
			expectedContent: []string{
				`"failure":"invalid price data"`,
			},
		},
		{
			name:           "checkout session invalid quantity",
			method:         http.MethodPost,
			url:            "/create-checkout-session",
			body:           `{"price":{"id":"price_test","type":"one_time"},"quantity":"two"}`,
			expectedStatus: http.StatusBadRequest,
			expectedContent: []string{
				`"failure":"invalid quantity"`,
			},
		},
		{
			name:           "checkout session missing auth",
			method:         http.MethodPost,
			url:            "/create-checkout-session",
			body:           `{"price":{"id":"price_test","type":"one_time"},"quantity":1}`,
			expectedStatus: http.StatusBadRequest,
			expectedContent: []string{
				`"failure":"could not find auth record by token"`,
			},
		},
		{
			name:           "checkout session one_time success",
			method:         http.MethodPost,
			url:            "/create-checkout-session",
			body:           `{"price":{"id":"price_test","type":"one_time"},"quantity":2}`,
			expectedStatus: http.StatusOK,
			expectedContent: []string{
				`"id":"cs_test"`,
			},
			setup: func(t testing.TB, app *tests.TestApp, scenario *tests.ApiScenario) {
				setupStripeMock(t)
				stripeSuccessURL = "https://example.com/success"
				stripeCancelURL = "https://example.com/cancel"
				ensureCustomerCollection(t, app)
				_, token := authTokenForTestUser(t, app)
				scenario.Headers = map[string]string{
					"Authorization": token,
				}
			},
			after: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				user, _ := authTokenForTestUser(t, app)
				record, err := app.FindFirstRecordByData("customer", "user_id", user.Id)
				if err != nil {
					t.Fatal(err)
				}
				if record.GetString("stripe_customer_id") != "cus_test" {
					t.Fatalf("Expected stripe_customer_id to be cus_test, got %s", record.GetString("stripe_customer_id"))
				}
			},
		},
		{
			name:           "checkout session recurring existing customer",
			method:         http.MethodPost,
			url:            "/create-checkout-session",
			body:           `{"price":{"id":"price_test","type":"recurring"},"quantity":1}`,
			expectedStatus: http.StatusOK,
			expectedContent: []string{
				`"id":"cs_test"`,
			},
			setup: func(t testing.TB, app *tests.TestApp, scenario *tests.ApiScenario) {
				setupStripeMock(t)
				stripeSuccessURL = "https://example.com/success"
				stripeCancelURL = "https://example.com/cancel"

				collection := ensureCustomerCollection(t, app)
				user, token := authTokenForTestUser(t, app)
				customerRecord := core.NewRecord(collection)
				customerRecord.Set("user_id", user.Id)
				customerRecord.Set("stripe_customer_id", "cus_existing")
				if err := app.Save(customerRecord); err != nil {
					t.Fatal(err)
				}
				scenario.Headers = map[string]string{
					"Authorization": token,
				}
			},
		},
	})
}

func TestCreatePortalLinkEndpoint(t *testing.T) {
	runEndpointScenarios(t, []endpointScenario{
		{
			name:           "portal link requires auth",
			method:         http.MethodPost,
			url:            "/create-portal-link",
			expectedStatus: http.StatusBadRequest,
			expectedContent: []string{
				`"failure":"could not find auth record by token"`,
			},
		},
		{
			name:           "portal link existing customer",
			method:         http.MethodPost,
			url:            "/create-portal-link",
			expectedStatus: http.StatusOK,
			expectedContent: []string{
				`"id":"bps_test"`,
			},
			setup: func(t testing.TB, app *tests.TestApp, scenario *tests.ApiScenario) {
				setupStripeMock(t)
				stripeBillingReturnURL = "https://example.com/return"

				collection := ensureCustomerCollection(t, app)
				user, token := authTokenForTestUser(t, app)
				customerRecord := core.NewRecord(collection)
				customerRecord.Set("user_id", user.Id)
				customerRecord.Set("stripe_customer_id", "cus_existing")
				if err := app.Save(customerRecord); err != nil {
					t.Fatal(err)
				}
				scenario.Headers = map[string]string{
					"Authorization": token,
				}
			},
		},
		{
			name:           "portal link new customer",
			method:         http.MethodPost,
			url:            "/create-portal-link",
			expectedStatus: http.StatusOK,
			expectedContent: []string{
				`"id":"bps_test"`,
			},
			setup: func(t testing.TB, app *tests.TestApp, scenario *tests.ApiScenario) {
				setupStripeMock(t)
				stripeBillingReturnURL = "https://example.com/return"
				ensureCustomerCollection(t, app)
				_, token := authTokenForTestUser(t, app)
				scenario.Headers = map[string]string{
					"Authorization": token,
				}
			},
			after: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				user, _ := authTokenForTestUser(t, app)
				record, err := app.FindFirstRecordByData("customer", "user_id", user.Id)
				if err != nil {
					t.Fatal(err)
				}
				if record.GetString("stripe_customer_id") != "cus_test" {
					t.Fatalf("Expected stripe_customer_id to be cus_test, got %s", record.GetString("stripe_customer_id"))
				}
			},
		},
	})
}

func TestStripeWebhookEndpoint(t *testing.T) {
	payloadUnknown := []byte(fmt.Sprintf(`{"id":"evt_test","object":"event","api_version":"%s","type":"invoice.created","data":{"object":{"id":"in_123"}}}`, stripe.APIVersion))
	signedUnknown := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: payloadUnknown,
		Secret:  "whsec_test",
	})

	payloadProduct := []byte(fmt.Sprintf(`{"id":"evt_test","object":"event","api_version":"%s","type":"product.created","data":{"object":{"id":"prod_test","object":"product","active":true,"name":"Test product","description":"Test desc","metadata":{"tier":"pro"}}}}`, stripe.APIVersion))
	signedProduct := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: payloadProduct,
		Secret:  "whsec_test",
	})

	runEndpointScenarios(t, []endpointScenario{
		{
			name:           "stripe webhook invalid signature",
			method:         http.MethodPost,
			url:            "/stripe",
			body:           `{"type":"product.created"}`,
			expectedStatus: http.StatusBadRequest,
			expectedContent: []string{
				"webhook verification failed",
			},
			headers: map[string]string{
				"Stripe-Signature": "t=123,v1=bad",
			},
			setup: func(t testing.TB, app *tests.TestApp, scenario *tests.ApiScenario) {
				WHSEC = "whsec_test"
			},
		},
		{
			name:           "stripe webhook unknown event",
			method:         http.MethodPost,
			url:            "/stripe",
			body:           string(payloadUnknown),
			expectedStatus: http.StatusBadRequest,
			expectedContent: []string{
				`"failure":"didn't receive a valid event"`,
			},
			headers: map[string]string{
				"Stripe-Signature": signedUnknown.Header,
			},
			setup: func(t testing.TB, app *tests.TestApp, scenario *tests.ApiScenario) {
				WHSEC = "whsec_test"
			},
		},
		{
			name:           "stripe webhook product created",
			method:         http.MethodPost,
			url:            "/stripe",
			body:           string(payloadProduct),
			expectedStatus: http.StatusOK,
			expectedContent: []string{
				`"success":"data was received"`,
			},
			headers: map[string]string{
				"Stripe-Signature": signedProduct.Header,
			},
			setup: func(t testing.TB, app *tests.TestApp, scenario *tests.ApiScenario) {
				WHSEC = "whsec_test"
				ensureProductCollection(t, app)
			},
			after: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				record, err := app.FindFirstRecordByData("product", "product_id", "prod_test")
				if err != nil {
					t.Fatal(err)
				}
				if record.GetString("name") != "Test product" {
					t.Fatalf("Expected product name to be Test product, got %s", record.GetString("name"))
				}
			},
		},
	})
}
