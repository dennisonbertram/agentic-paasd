-- Add port column to services table
ALTER TABLE services ADD COLUMN port INTEGER NOT NULL DEFAULT 8000;

-- Environment variables for services, encrypted with AES-256-GCM
CREATE TABLE IF NOT EXISTS service_env (
  service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
  key TEXT NOT NULL,
  value_encrypted TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (service_id, key)
);
