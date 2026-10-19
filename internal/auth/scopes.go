package auth

// Scopes defines the available authorization scopes for the proxy.
// Scope constants are defined in context.go

// ScopeHierarchy defines parent-child relationships for scope inheritance.
var ScopeHierarchy = map[string][]string{
	"admin": {
		ScopeAdminUsersWrite,
		ScopeAdminKeysWrite,
		ScopeAdminQuotasWrite,
		ScopeAdminProvidersWrite,
		ScopeAdminUsageRead,
		ScopeAdminAuditRead,
	},
}
