# 1. Write our own GitHub webhook handler

Date: 2026-09-30

## Status

Accepted

## Context

gauger-server receives `workflow_run` and `workflow_job` webhooks on the Funnel listener and stores runs, jobs and steps in Postgres. The spec asks us to choose between embedding the OpenTelemetry Collector's `githubreceiver` and writing a handler.

`githubreceiver` is alpha for both metrics and traces. Its webhook side verifies `X-Hub-Signature-256` and turns each event into trace spans with deterministic IDs. It runs as a Collector component, so its output goes to a Collector pipeline and an exporter.

The spec needs things the receiver does not do:

- Deduplicate on `X-GitHub-Delivery`. The receiver turns a redelivery into a second span.
- Repair missed deliveries from the REST API, with the same upsert rules as the webhook path.
- Write to our Postgres tables and keep job status from moving backwards when events arrive out of order.

## Decision

Write the handler in `internal/webhook`. It reads the body with a size limit, checks the HMAC before parsing any JSON, records the delivery ID in the same transaction as the upserts, and shares its upsert code with the REST repair worker.

## Consequences

- The handler needs a few hundred lines of Go and tests, and no Collector dependency tree.
- We decode only the payload fields we store. New fields need a code change.
- Our spans do not share the receiver's deterministic trace IDs. If we later export traces, we can compute those IDs from `run_id`, `run_attempt` and `check_run_id` ourselves.
