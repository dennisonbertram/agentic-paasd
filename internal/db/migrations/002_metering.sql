CREATE TABLE IF NOT EXISTS usage_events (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id),
  service_id TEXT,
  event_type TEXT NOT NULL,
  cpu_seconds REAL NOT NULL DEFAULT 0,
  memory_mb_seconds REAL NOT NULL DEFAULT 0,
  network_ingress_bytes INTEGER NOT NULL DEFAULT 0,
  network_egress_bytes INTEGER NOT NULL DEFAULT 0,
  disk_bytes INTEGER NOT NULL DEFAULT 0,
  recorded_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_daily (
  tenant_id TEXT NOT NULL,
  service_id TEXT,
  date TEXT NOT NULL,
  cpu_seconds REAL NOT NULL DEFAULT 0,
  memory_mb_seconds REAL NOT NULL DEFAULT 0,
  network_ingress_bytes INTEGER NOT NULL DEFAULT 0,
  network_egress_bytes INTEGER NOT NULL DEFAULT 0,
  disk_bytes INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, date, service_id)
);
CREATE INDEX IF NOT EXISTS idx_usage_events_tenant ON usage_events(tenant_id);
CREATE INDEX IF NOT EXISTS idx_usage_events_recorded ON usage_events(recorded_at);
CREATE INDEX IF NOT EXISTS idx_usage_daily_tenant ON usage_daily(tenant_id, date);
