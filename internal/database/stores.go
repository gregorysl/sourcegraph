package database

// Global reference to database stores using the global dbconn.Global connection handle.
// Deprecated: Use store constructors instead.
var (
	GlobalExternalServices            = &ExternalServiceStore{}
	GlobalRepos                       = &RepoStore{}
	GlobalUsers                       = &UserStore{}
	GlobalUserEmails                  = &UserEmailsStore{}
	GlobalAuthz            AuthzStore = &authzStore{}
)
