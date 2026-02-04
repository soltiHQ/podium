package domain

import "time"

var _ Entity[*CredentialModel] = (*CredentialModel)(nil)

// CredentialType defines the authentication mechanism type.
type CredentialType string

const (
	CredentialTypePassword CredentialType = "password"
	CredentialTypeAPIKey   CredentialType = "api_key"
)

// CredentialModel is a domain model that describes user authentication credentials.
type CredentialModel struct {
	createdAt time.Time
	updatedAt time.Time

	userID string
	id     string

	data     map[string]string
	credType CredentialType
}

// NewCredentialModel creates a new credential domain model.
func NewCredentialModel(id, userID string, credType CredentialType) (*CredentialModel, error) {
	if id == "" || userID == "" {
		return nil, ErrEmptyID
	}
	var (
		now  = time.Now()
		data = make(map[string]string)
	)
	return &CredentialModel{
		createdAt: now,
		updatedAt: now,
		id:        id,
		userID:    userID,
		credType:  credType,
		data:      data,
	}, nil
}

// ID returns the unique identifier for this credential.
func (c *CredentialModel) ID() string {
	return c.id
}

// UserID returns the ID of the user this credential belongs to.
func (c *CredentialModel) UserID() string {
	return c.userID
}

// Type returns the credential type.
func (c *CredentialModel) Type() CredentialType {
	return c.credType
}

// GetData returns a credential data value by key.
func (c *CredentialModel) GetData(key string) (string, bool) {
	val, ok := c.data[key]
	return val, ok
}

// SetData sets a credential data value.
func (c *CredentialModel) SetData(key, value string) {
	c.data[key] = value
	c.updatedAt = time.Now()
}

// CreatedAt returns the creation timestamp.
func (c *CredentialModel) CreatedAt() time.Time {
	return c.createdAt
}

// UpdatedAt returns the last modification timestamp.
func (c *CredentialModel) UpdatedAt() time.Time {
	return c.updatedAt
}

// Clone creates a deep copy of the credential model.
func (c *CredentialModel) Clone() *CredentialModel {
	clonedData := make(map[string]string, len(c.data))
	for k, v := range c.data {
		clonedData[k] = v
	}
	return &CredentialModel{
		id:        c.id,
		userID:    c.userID,
		credType:  c.credType,
		data:      clonedData,
		createdAt: c.createdAt,
		updatedAt: c.updatedAt,
	}
}
