-- =========================
-- 005_shapes_indexes.sql
-- Speeds up shape polyline reads (filter by shape_id + order by sequence).
-- The GTFS shapes table is large; each API call only touches one shape_id.
-- =========================

CREATE INDEX IF NOT EXISTS idx_shapes_shape_id_sequence
ON shapes (shape_id, shape_pt_sequence);

ANALYZE shapes;
