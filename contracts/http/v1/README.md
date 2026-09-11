# Neonix HTTP contract fixtures

This directory describes the externally observable HTTP boundary that the Go backend must preserve for the Neonix Next.js UI, local clients, and workers.

Fixtures are deliberately transport-focused. They describe method, path, authentication boundary, success shape, structured error shape, and streaming/cancellation expectations without embedding credentials or upstream response text.

The contract harness should run against an `httptest.Server` and a fake provider registry. Provider tests must use the same fixture format so that a provider slice cannot pass by testing only an internal service.
