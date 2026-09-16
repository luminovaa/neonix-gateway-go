-- Neonix is a private single-operator deployment. Disable the inherited
-- Sub2API resale-margin admission gate for all existing groups. The columns
-- remain temporarily readable so a mixed-version rollback is still safe.
UPDATE groups
SET profit_control_enabled = FALSE,
    profit_min_margin = 0,
    profit_safety_buffer = 0
WHERE profit_control_enabled = TRUE
   OR profit_min_margin <> 0
   OR profit_safety_buffer <> 0;
