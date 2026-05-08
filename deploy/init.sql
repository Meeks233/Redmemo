-- RedMemo PostgreSQL initialization
-- Tables are auto-migrated by the app; this file handles extensions and tuning.

-- Useful for full-text search on archived posts (future)
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Tuning for JSONB-heavy workload
ALTER SYSTEM SET shared_buffers = '256MB';
ALTER SYSTEM SET work_mem = '16MB';
ALTER SYSTEM SET maintenance_work_mem = '128MB';
ALTER SYSTEM SET effective_cache_size = '768MB';
