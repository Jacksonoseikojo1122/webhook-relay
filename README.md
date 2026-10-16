# Webhook Relay

Receive, verify, de-duplicate and retry webhooks so no payment event is lost or handled twice.

> **Status: in development.** I'm building this in public. Follow along here or on the project page:
> **https://jacksonkojoosei.com/projects/webhook-relay**

## What it does

A small service that sits in front of your app and takes the webhooks from Paystack, Flutterwave, Stripe and mobile-money providers. It checks the signature, drops duplicates, stores every event, and delivers it to your app with retries until your app says it got it.

## Why

Payment webhooks arrive late, twice, or out of order, and your server is sometimes down when they arrive. Most apps handle this badly, which is how customers get charged twice or never get what they paid for.

## Planned features

- [ ] Signature checks for Paystack, Flutterwave and Stripe
- [ ] Duplicate detection by provider event ID
- [ ] Durable event log with replay
- [ ] Delivery to your endpoint with exponential backoff
- [ ] Dashboard of failed deliveries and why they failed
- [ ] OpenTelemetry traces from receipt to delivery

## Stack

Go, Redis Streams, PostgreSQL, OpenTelemetry, Next.js, Docker

## Author

Built by **[Jackson Kojo Osei](https://jacksonkojoosei.com)**, CTO in Accra, Ghana.
[LinkedIn](https://www.linkedin.com/in/jackson-kojo-osei-740846189) · [All projects](https://jacksonkojoosei.com/projects)

## License

MIT
