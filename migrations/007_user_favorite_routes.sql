-- Favorite routes per user (one row per user_id + route_id).
CREATE TABLE IF NOT EXISTS user_favorite_routes (
    user_id UUID NOT NULL,
    route_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT user_favorite_routes_pkey PRIMARY KEY (user_id, route_id)
);

CREATE INDEX IF NOT EXISTS idx_user_favorite_routes_user
ON user_favorite_routes (user_id, created_at DESC);
