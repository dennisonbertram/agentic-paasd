CREATE TABLE IF NOT EXISTS builds (
  id TEXT PRIMARY KEY,
  service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
  tenant_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  source_type TEXT NOT NULL,
  source_url TEXT,
  source_ref TEXT DEFAULT 'main',
  image TEXT,
  nixpacks_plan TEXT,
  log TEXT DEFAULT '',
  started_at INTEGER,
  finished_at INTEGER,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_builds_service ON builds(service_id);
CREATE INDEX IF NOT EXISTS idx_builds_tenant ON builds(tenant_id);
