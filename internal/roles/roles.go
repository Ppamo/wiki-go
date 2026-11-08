package roles

// Role constants for user permissions
const (
	// RoleAdmin can do anything from document actions to changing settings and creating/deleting comments
	RoleAdmin = "admin"
	
	// RoleEditor can only do document actions and post comments
	RoleEditor = "editor"
	
	// RoleViewer can only view documents and post comments
	RoleViewer = "viewer"
)

// Rank orders roles from least to most privileged: admin > editor > viewer.
// Unknown roles rank 0.
func Rank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}
