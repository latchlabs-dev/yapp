# Client onboarding

POST /admin/keys creates a user key and returns onboarding payloads: a TypeScript extension,
a models.json, and a curl test. PROXY_PUBLIC_BASE_URL must be set or onboarding generation fails.
Keys use the cpk_live_ prefix. Regenerate onboarding for an existing key via
POST /tenants/{id}/users/{id}/onboarding.
