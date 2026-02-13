# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder

WORKDIR /app
ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /out/app ./main.go

FROM alpine:3.20

WORKDIR /app
RUN apk add --no-cache bash ca-certificates

ARG STRIPE_SECRET_KEY=""
ARG STRIPE_CANCEL_URL=""
ARG STRIPE_SUCCESS_URL=""
ARG STRIPE_BILLING_RETURN_URL=""
ARG STRIPE_WHSEC=""
ARG HOST=""
ARG PORT="443"
ARG DEVELOPMENT=""

ENV STRIPE_SECRET_KEY=${STRIPE_SECRET_KEY}
ENV STRIPE_CANCEL_URL=${STRIPE_CANCEL_URL}
ENV STRIPE_SUCCESS_URL=${STRIPE_SUCCESS_URL}
ENV STRIPE_BILLING_RETURN_URL=${STRIPE_BILLING_RETURN_URL}
ENV STRIPE_WHSEC=${STRIPE_WHSEC}
ENV HOST=${HOST}
ENV PORT=${PORT}
ENV DEVELOPMENT=${DEVELOPMENT}

COPY --from=builder /out/app /app/bin/app
COPY ./script.sh /script.sh
COPY ./hooks /app/hooks
COPY ./pb_bootstrap /app/pb_bootstrap
COPY ./stripe_bootstrap /app/stripe_bootstrap

RUN chmod +x /script.sh

CMD ["/script.sh"]
