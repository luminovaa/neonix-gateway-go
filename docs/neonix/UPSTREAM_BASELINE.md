# Neonix Go gateway upstream baseline

- Upstream: `https://github.com/Wei-Shaw/sub2api`
- Release: `v0.2.4`
- Resolved upstream commit: `5de5e2bed035d43591a2e10e51f420ef6a84eb98`
- Fork: `https://github.com/luminovaa/neonix-gateway-go`
- Migration branch: `neonix/migration-v0.2.4`
- Product UI: the existing Neonix Next.js application remains authoritative.
- Runtime target: this repository becomes the Neonix Go control plane and gateway data plane after the provider slices are complete.
- Worker boundary: Python automation remains a separate worker and is not embedded into this HTTP server.

## Upstream update policy

Upstream changes are imported only through a reviewed branch. The imported commit must be recorded here, the Neonix contract fixtures must pass, and provider-specific behavior must be checked before it is promoted to the migration branch.

## Product exclusions

The target Neonix runtime is single-operator. Billing, payments, subscriptions, invitation codes, member/team management, tenant ownership, and the Sub2API Vue product UI are not part of the Neonix product surface. Their removal is handled by the staged migration issues rather than by changing the pinned baseline silently.
