CREATE TABLE IF NOT EXISTS databases (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  type TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'provisioning',
  container_id TEXT,
  host TEXT DEFAULT '127.0.0.1',
  port INTEGER,
  db_name TEXT,
  username TEXT,
  password_encrypted TEXT NOT NULL,
  connection_string_encrypted TEXT,
  volume_name TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_databases_tenant ON databases(tenant_id);
