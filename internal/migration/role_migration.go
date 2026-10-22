package migration

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"
	"wiki-go/internal/logger"
	"wiki-go/internal/roles"

	"gopkg.in/yaml.v3"
)

// copyFile creates a copy of src file at dst path
func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()

	_, err = io.Copy(destination, source)
	return err
}

// MigrateUserRoles updates the user configuration from the old IsAdmin boolean
// to the new role-based system. This function should be called during application
// startup to ensure all users have a proper role assigned.
//
// The config is edited as a YAML node tree so that only the affected user
// entries change; every other setting (access rules, groups, security, and
// any field this package does not know about) is written back untouched.
func MigrateUserRoles(configPath string) error {
	logger.Debug("Checking if user role migration is needed...")

	// Read the config file
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Config file does not exist yet – nothing to migrate. It will be
			// created with defaults when the main application loads the config.
			logger.Info("Config file %s not found, skipping user-role migration", configPath)
			return nil
		}
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse the config
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("failed to parse config file: %w", err)
	}

	users := usersNode(&doc)
	if users == nil {
		logger.Debug("No users found in config, skipping user-role migration")
		return nil
	}

	// Migrate each user in place
	migrationNeeded := false
	for _, user := range users.Content {
		if user.Kind != yaml.MappingNode {
			continue
		}
		username, role, migrated, err := migrateUserNode(user)
		if err != nil {
			return err
		}
		if migrated {
			migrationNeeded = true
			logger.Info("Migrated user %s: Role=%s", username, role)
		}
	}

	if !migrationNeeded {
		logger.Debug("No role migration needed, all users have roles assigned")
		return nil
	}

	// Create backup of config file before migration (only if migration is needed)
	backupPath := configPath + "." + time.Now().Format("20060102-150405") + ".bak"
	if err := copyFile(configPath, backupPath); err != nil {
		return fmt.Errorf("failed to create config backup: %w", err)
	}
	logger.Info("Created config backup at: %s", backupPath)

	logger.Info("Migrating users from IsAdmin to role-based system...")

	// Save the updated config
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(4)
	if err := encoder.Encode(&doc); err != nil {
		return fmt.Errorf("failed to marshal updated config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("failed to marshal updated config: %w", err)
	}

	if err := os.WriteFile(configPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to save migrated config: %w", err)
	}

	logger.Info("User role migration completed successfully")
	return nil
}

// usersNode returns the sequence node holding the top-level "users" list, or
// nil when the document has none.
func usersNode(doc *yaml.Node) *yaml.Node {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "users" && root.Content[i+1].Kind == yaml.SequenceNode {
			return root.Content[i+1]
		}
	}
	return nil
}

// migrateUserNode assigns a role to a user mapping that has no role or still
// carries the old is_admin flag, and removes is_admin. It reports whether the
// node was changed.
func migrateUserNode(user *yaml.Node) (username, role string, migrated bool, err error) {
	var roleValue *yaml.Node
	isAdmin := false
	isAdminIndex := -1

	for i := 0; i+1 < len(user.Content); i += 2 {
		key, value := user.Content[i], user.Content[i+1]
		switch key.Value {
		case "username":
			username = value.Value
		case "role":
			roleValue = value
			role = value.Value
		case "is_admin":
			isAdminIndex = i
			if value.Tag != "!!null" {
				if err := value.Decode(&isAdmin); err != nil {
					return "", "", false, fmt.Errorf("invalid is_admin value for user %s: %w", username, err)
				}
			}
		}
	}

	// If role is already set and no IsAdmin field, keep it
	if role != "" && !isAdmin {
		return username, role, false, nil
	}

	// Set role based on IsAdmin field if present
	if isAdmin {
		role = roles.RoleAdmin
	} else {
		role = roles.RoleViewer
	}

	if roleValue != nil {
		roleValue.Kind = yaml.ScalarNode
		roleValue.Tag = "!!str"
		roleValue.Style = 0
		roleValue.Value = role
	}

	// Clear the IsAdmin field after migration
	if isAdminIndex >= 0 {
		user.Content = append(user.Content[:isAdminIndex], user.Content[isAdminIndex+2:]...)
	}

	if roleValue == nil {
		user.Content = append(user.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "role"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: role},
		)
	}

	return username, role, true, nil
}
