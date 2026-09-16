-- Neonix is a private single-operator deployment. Model availability is
-- determined by provider/account health, never by a paid application role.
ALTER TABLE neonix_model_catalog DROP COLUMN IF EXISTS requires_pro;
