package gateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"conduit/sdk"

	_ "modernc.org/sqlite"
)

// DB stores connector instances and credentials in SQLite, encrypting secrets with AES-256-GCM.
type DB struct {
	conn          *sql.DB
	encryptionKey []byte
}

// ConnectorInstance is one installed connector. BaselineSchema is the response shape
// captured at install, as JSON.
type ConnectorInstance struct {
	ID             string    `json:"id"`
	ConnectorID    string    `json:"connector_id"`
	Status         string    `json:"status"` // "active", "drifted", "error"
	BaselineSchema string    `json:"baseline_schema"`
	CreatedAt      time.Time `json:"created_at"`
}

// NewDB opens the SQLite file at dbPath and creates missing tables. The encryption key
// comes from CONDUIT_ENCRYPTION_KEY.
func NewDB(dbPath string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}
	// SQLite allows one writer, and every ":memory:" connection would be its own empty
	// database, so use a single connection.
	sqlDB.SetMaxOpenConns(1)

	db := &DB{
		conn:          sqlDB,
		encryptionKey: encryptionKeyFromEnv(),
	}

	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return db, nil
}

// Close closes the database.
func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS connector_instances (
			id TEXT PRIMARY KEY,
			connector_id TEXT NOT NULL,
			status TEXT NOT NULL,
			baseline_schema TEXT,
			created_at DATETIME NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS credentials (
			instance_id TEXT PRIMARY KEY,
			auth_type TEXT NOT NULL,
			encrypted_access_token TEXT,
			encrypted_refresh_token TEXT,
			token_expiry DATETIME,
			encrypted_api_key TEXT,
			FOREIGN KEY(instance_id) REFERENCES connector_instances(id) ON DELETE CASCADE
		);`,
	}

	for _, q := range queries {
		if _, err := db.conn.Exec(q); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
	}

	// Older databases stored the webhook secret per instance in plain text.
	var hasSecretColumn int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('connector_instances') WHERE name = 'webhook_secret';`).Scan(&hasSecretColumn); err != nil {
		return fmt.Errorf("inspect connector_instances: %w", err)
	}
	if hasSecretColumn > 0 {
		if _, err := db.conn.Exec(`UPDATE connector_instances SET webhook_secret = NULL;`); err != nil {
			return fmt.Errorf("clear stored webhook secrets: %w", err)
		}
	}
	return nil
}

// encryptionKeyFromEnv exits in production when the key is missing or malformed, because a
// weak key would leave credentials readable; development falls back to a fixed key.
func encryptionKeyFromEnv() []byte {
	keyStr := os.Getenv("CONDUIT_ENCRYPTION_KEY")
	isProd := os.Getenv("CONDUIT_ENV") == "production"

	if keyStr == "" {
		if isProd {
			slog.Error("CONDUIT_ENCRYPTION_KEY is not set in production", "component", "db")
			os.Exit(1)
		}
		slog.Warn("CONDUIT_ENCRYPTION_KEY is not set, using the development key", "component", "db")
		keyStr = "dev_encryption_key_32_bytes_long"
	}

	if key, ok := parseEncryptionKey(keyStr); ok {
		return key
	}
	if isProd {
		slog.Error("CONDUIT_ENCRYPTION_KEY must be base64 of 32 bytes or exactly 32 characters", "component", "db", "length", len(keyStr))
		os.Exit(1)
	}
	slog.Warn("CONDUIT_ENCRYPTION_KEY is not a valid 32-byte key, padding or truncating it for development", "component", "db", "length", len(keyStr))
	padded := make([]byte, 32)
	copy(padded, keyStr)
	return padded
}

// parseEncryptionKey accepts base64 of 32 bytes (`openssl rand -base64 32`) or a raw
// 32-byte string; they cannot be confused, since 32 base64 characters decode to 24 bytes.
func parseEncryptionKey(s string) ([]byte, bool) {
	if key, err := base64.StdEncoding.DecodeString(s); err == nil && len(key) == 32 {
		return key, true
	}
	if len(s) == 32 {
		return []byte(s), true
	}
	return nil, false
}

// encrypt returns base64(nonce||ciphertext) using a fresh nonce per call.
func (db *DB) encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	block, err := aes.NewCipher(db.encryptionKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (db *DB) decrypt(ciphertextStr string) (string, error) {
	if ciphertextStr == "" {
		return "", nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextStr)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(db.encryptionKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	nonce, actualCiphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, actualCiphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// execer is satisfied by both *sql.DB and *sql.Tx, so writes can run in a transaction.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// CreateInstance stores a new instance and its credentials in one transaction, so a
// failed credentials write never leaves a half-installed instance behind.
func (db *DB) CreateInstance(inst *ConnectorInstance, authType sdk.AuthType, creds *sdk.Credentials) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("create instance %s: %w", inst.ID, err)
	}
	defer tx.Rollback()

	if err := saveInstance(tx, inst); err != nil {
		return err
	}
	if err := db.insertCredentials(tx, inst.ID, authType, creds); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create instance %s: %w", inst.ID, err)
	}
	return nil
}

func saveInstance(ex execer, inst *ConnectorInstance) error {
	query := `INSERT INTO connector_instances (id, connector_id, status, baseline_schema, created_at)
		VALUES (?, ?, ?, ?, ?);`
	if _, err := ex.Exec(query, inst.ID, inst.ConnectorID, inst.Status, inst.BaselineSchema, inst.CreatedAt); err != nil {
		return fmt.Errorf("save instance %s: %w", inst.ID, err)
	}
	return nil
}

// SetInstanceStatus updates the status of an existing instance and reports whether it
// still exists. It never inserts, so a drift scan cannot bring back a deleted instance.
func (db *DB) SetInstanceStatus(id, status string) (bool, error) {
	res, err := db.conn.Exec(`UPDATE connector_instances SET status = ? WHERE id = ?;`, status, id)
	if err != nil {
		return false, fmt.Errorf("set status of %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set status of %s: %w", id, err)
	}
	return n == 1, nil
}

// Instance returns the instance with the given ID.
func (db *DB) Instance(id string) (*ConnectorInstance, error) {
	query := `SELECT id, connector_id, status, baseline_schema, created_at FROM connector_instances WHERE id = ?;`
	row := db.conn.QueryRow(query, id)

	var inst ConnectorInstance
	err := row.Scan(&inst.ID, &inst.ConnectorID, &inst.Status, &inst.BaselineSchema, &inst.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("instance not found: %s", id)
		}
		return nil, fmt.Errorf("get instance %s: %w", id, err)
	}
	return &inst, nil
}

// ListInstances returns all instances, newest first.
func (db *DB) ListInstances() ([]*ConnectorInstance, error) {
	query := `SELECT id, connector_id, status, baseline_schema, created_at FROM connector_instances ORDER BY created_at DESC;`
	rows, err := db.conn.Query(query)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer rows.Close()

	var list []*ConnectorInstance
	for rows.Next() {
		var inst ConnectorInstance
		if err := rows.Scan(&inst.ID, &inst.ConnectorID, &inst.Status, &inst.BaselineSchema, &inst.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan instance: %w", err)
		}
		list = append(list, &inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	return list, nil
}

// DeleteInstance removes the instance and its credentials in one transaction. The
// credentials are deleted explicitly because SQLite ignores ON DELETE CASCADE by default.
func (db *DB) DeleteInstance(id string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("delete instance %s: %w", id, err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM credentials WHERE instance_id = ?;`, id); err != nil {
		return fmt.Errorf("delete credentials for %s: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM connector_instances WHERE id = ?;`, id); err != nil {
		return fmt.Errorf("delete instance %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete instance %s: %w", id, err)
	}
	return nil
}

// errNoCredentials is returned when credentials are updated for an instance that has
// none, usually because the instance was deleted.
var errNoCredentials = errors.New("no stored credentials")

// UpdateCredentials replaces the stored credentials of an existing instance. It never
// inserts, so a token refresh that finishes after a delete cannot store secrets again.
func (db *DB) UpdateCredentials(instanceID string, authType sdk.AuthType, creds *sdk.Credentials) error {
	enc, err := db.encryptCredentials(creds)
	if err != nil {
		return err
	}
	res, err := db.conn.Exec(`UPDATE credentials SET auth_type = ?, encrypted_access_token = ?,
		encrypted_refresh_token = ?, token_expiry = ?, encrypted_api_key = ? WHERE instance_id = ?;`,
		string(authType), enc.token, enc.refresh, enc.expiry, enc.apiKey, instanceID)
	if err != nil {
		return fmt.Errorf("update credentials for %s: %w", instanceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update credentials for %s: %w", instanceID, err)
	}
	if n == 0 {
		return fmt.Errorf("update credentials for %s: %w", instanceID, errNoCredentials)
	}
	return nil
}

func (db *DB) insertCredentials(ex execer, instanceID string, authType sdk.AuthType, creds *sdk.Credentials) error {
	enc, err := db.encryptCredentials(creds)
	if err != nil {
		return err
	}
	query := `INSERT INTO credentials (instance_id, auth_type, encrypted_access_token, encrypted_refresh_token, token_expiry, encrypted_api_key)
		VALUES (?, ?, ?, ?, ?, ?);`
	if _, err := ex.Exec(query, instanceID, string(authType), enc.token, enc.refresh, enc.expiry, enc.apiKey); err != nil {
		return fmt.Errorf("save credentials for %s: %w", instanceID, err)
	}
	return nil
}

type encryptedCredentials struct {
	token, refresh, apiKey string
	expiry                 any // nil is stored as NULL for tokens without an expiry
}

func (db *DB) encryptCredentials(creds *sdk.Credentials) (encryptedCredentials, error) {
	var enc encryptedCredentials
	var err error
	if creds.Token != nil {
		if enc.token, err = db.encrypt(creds.Token.AccessToken); err != nil {
			return enc, fmt.Errorf("failed to encrypt access token: %w", err)
		}
		if enc.refresh, err = db.encrypt(creds.Token.RefreshToken); err != nil {
			return enc, fmt.Errorf("failed to encrypt refresh token: %w", err)
		}
		if !creds.Token.Expiry.IsZero() {
			enc.expiry = creds.Token.Expiry
		}
	}
	if creds.APIKey != "" {
		if enc.apiKey, err = db.encrypt(creds.APIKey); err != nil {
			return enc, fmt.Errorf("failed to encrypt api key: %w", err)
		}
	}
	return enc, nil
}

// Credentials returns the decrypted credentials and the auth type they were stored with.
func (db *DB) Credentials(instanceID string) (*sdk.Credentials, sdk.AuthType, error) {
	query := `SELECT auth_type, encrypted_access_token, encrypted_refresh_token, token_expiry, encrypted_api_key FROM credentials WHERE instance_id = ?;`
	row := db.conn.QueryRow(query, instanceID)

	var authTypeStr string
	var encToken, encRefresh, encAPIKey sql.NullString
	var tokenExpiry sql.NullTime

	err := row.Scan(&authTypeStr, &encToken, &encRefresh, &tokenExpiry, &encAPIKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", fmt.Errorf("credentials not found for instance: %s", instanceID)
		}
		return nil, "", fmt.Errorf("get credentials for %s: %w", instanceID, err)
	}

	authType := sdk.AuthType(authTypeStr)
	var creds sdk.Credentials

	if authType == sdk.AuthTypeOAuth2 && encToken.Valid {
		decToken, err := db.decrypt(encToken.String)
		if err != nil {
			return nil, "", fmt.Errorf("failed to decrypt access token: %w", err)
		}
		decRefresh, err := db.decrypt(encRefresh.String)
		if err != nil {
			return nil, "", fmt.Errorf("failed to decrypt refresh token: %w", err)
		}

		var expiry time.Time
		if tokenExpiry.Valid {
			expiry = tokenExpiry.Time
		}

		creds.Token = &sdk.Token{
			AccessToken:  decToken,
			RefreshToken: decRefresh,
			Expiry:       expiry,
		}
	} else if authType == sdk.AuthTypeAPIKey && encAPIKey.Valid {
		decAPIKey, err := db.decrypt(encAPIKey.String)
		if err != nil {
			return nil, "", fmt.Errorf("failed to decrypt api key: %w", err)
		}
		creds.APIKey = decAPIKey
	}

	return &creds, authType, nil
}
