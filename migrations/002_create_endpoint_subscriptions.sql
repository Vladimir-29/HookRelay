CREATE TABLE endpoint_subscriptions (
    endpoint_id BIGINT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    event_type  TEXT   NOT NULL,
    PRIMARY KEY (endpoint_id, event_type)
);

CREATE INDEX idx_endpoint_subscriptions_event_type
    ON endpoint_subscriptions (event_type);
