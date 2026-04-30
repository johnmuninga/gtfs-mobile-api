-- Range and per-service lookups on GTFS calendar_dates (DB-backed, not txt).
CREATE INDEX IF NOT EXISTS idx_calendar_dates_service_date
ON calendar_dates (service_id, date);

CREATE INDEX IF NOT EXISTS idx_calendar_dates_date
ON calendar_dates (date);

ANALYZE calendar_dates;
