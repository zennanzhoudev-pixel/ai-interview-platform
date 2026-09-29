package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MySQLKeyStore 把 API Key 持久化到 MySQL。
//
// 为什么不沿用内存实现: 密钥一旦重启就丢失, 意味着每次发布都要重新给
// 客户换一次密钥 —— 这在生产上是不可接受的。库表只存 sha256 哈希,
// 明文不落库。
type MySQLKeyStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewMySQLKeyStore 构造 MySQL 密钥库。
func NewMySQLKeyStore(db *sql.DB) *MySQLKeyStore {
	return &MySQLKeyStore{db: db, now: time.Now}
}

// Add 创建密钥并返回明文(仅此一次)。
func (m *MySQLKeyStore) Add(tenantID, name string, role Role) (string, Principal, error) {
	raw, err := GenerateKey()
	if err != nil {
		return "", Principal{}, err
	}
	hash := HashKey(raw)
	keyID := "key_" + hash[:12]
	_, err = m.db.ExecContext(context.Background(), `
		INSERT INTO tenant_api_key (key_id, tenant_id, name, role, key_hash, created_at)
		VALUES (?,?,?,?,?,?)`,
		keyID, tenantID, name, string(role), hash, m.now().UTC())
	if err != nil {
		return "", Principal{}, err
	}
	return raw, Principal{TenantID: tenantID, KeyID: keyID, Role: role, Name: name}, nil
}

// Resolve 实现 KeyStore。
func (m *MySQLKeyStore) Resolve(ctx context.Context, rawKey string) (Principal, error) {
	if rawKey == "" {
		return Principal{}, ErrInvalidKey
	}
	var (
		keyID, tenantID, role, name string
		revokedAt                   sql.NullTime
	)
	err := m.db.QueryRowContext(ctx, `
		SELECT key_id, tenant_id, role, name, revoked_at
		FROM tenant_api_key WHERE key_hash=?`, HashKey(rawKey)).
		Scan(&keyID, &tenantID, &role, &name, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrInvalidKey
	}
	if err != nil {
		return Principal{}, err
	}
	if revokedAt.Valid {
		return Principal{}, ErrInvalidKey
	}
	return Principal{TenantID: tenantID, KeyID: keyID, Role: Role(role), Name: name}, nil
}

// List 返回全部密钥记录(不含明文)。管理接口会自行按租户过滤。
func (m *MySQLKeyStore) List() []KeyRecord {
	rows, err := m.db.QueryContext(context.Background(), `
		SELECT key_id, tenant_id, role, name, key_hash, created_at, revoked_at
		FROM tenant_api_key ORDER BY created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []KeyRecord
	for rows.Next() {
		var (
			rec       KeyRecord
			role      string
			revokedAt sql.NullTime
		)
		if err := rows.Scan(&rec.KeyID, &rec.TenantID, &role, &rec.Name,
			&rec.KeyHash, &rec.CreatedAt, &revokedAt); err != nil {
			return out
		}
		rec.Role = Role(role)
		if revokedAt.Valid {
			rec.RevokedAt = revokedAt.Time
		}
		out = append(out, rec)
	}
	return out
}

// Revoke 吊销密钥。
func (m *MySQLKeyStore) Revoke(keyID string) bool {
	res, err := m.db.ExecContext(context.Background(), `
		UPDATE tenant_api_key SET revoked_at=? WHERE key_id=? AND revoked_at IS NULL`,
		m.now().UTC(), keyID)
	if err != nil {
		return false
	}
	n, err := res.RowsAffected()
	return err == nil && n > 0
}

// CountForTenant 返回某租户的有效密钥数, 用于首次启动时判断是否需要引导密钥。
func (m *MySQLKeyStore) CountForTenant(tenantID string) (int, error) {
	var n int
	err := m.db.QueryRowContext(context.Background(), `
		SELECT COUNT(1) FROM tenant_api_key WHERE tenant_id=? AND revoked_at IS NULL`,
		tenantID).Scan(&n)
	return n, err
}
