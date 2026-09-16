-- Provider credentials intentionally remain in accounts.credentials for the
-- single-operator Neonix deployment. The Go runtime no longer reads or writes
-- the provider-envelope handoff table introduced by migration 238.
--
-- Do not modify migration 238: deployed databases retain its checksum. This
-- forward-only migration is applied only after a bridge Go image that has no
-- dependency on this table has passed readiness and gateway smoke tests.
DROP TABLE IF EXISTS account_credential_envelopes;
