# Quota system design

Quota enforcement uses Redis-backed reservations (internal/quota/enforcer.go). CheckAndReserve
reserves concurrency and spend atomically before the upstream call; CompleteReservation settles
actual token usage afterwards. Reservations expire via PROXY_QUOTA_RESERVATION_TTL (15m) so
crashed requests release their hold. Policies are per tenant and per key, refreshed from the
quota_policies table every PROXY_PROVIDERS_REFRESH_INTERVAL. Concurrent requests racing the same
budget are serialized by Redis Lua scripts - never check-then-set from Go.
