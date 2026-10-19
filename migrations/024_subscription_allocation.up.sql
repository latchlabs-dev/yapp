CREATE TABLE subscription_billing_periods (
    id BIGSERIAL PRIMARY KEY,
    provider_account_id TEXT NOT NULL REFERENCES provider_accounts(id),
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL CHECK (ends_at > starts_at),
    amount_millicents BIGINT NOT NULL CHECK (amount_millicents > 0),
    allocated_millicents BIGINT NOT NULL DEFAULT 0 CHECK (allocated_millicents >= 0 AND allocated_millicents <= amount_millicents),
    unattributed_millicents BIGINT NOT NULL DEFAULT 0 CHECK (unattributed_millicents >= 0 AND unattributed_millicents <= allocated_millicents),
    consumed_points NUMERIC NOT NULL DEFAULT 0,
    observed_through TIMESTAMPTZ,
    revision BIGINT NOT NULL DEFAULT 0,
    evidence_hash TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(provider_account_id, starts_at)
);

CREATE TABLE subscription_allocation_adjustments (
    id BIGSERIAL PRIMARY KEY,
    period_id BIGINT NOT NULL REFERENCES subscription_billing_periods(id),
    revision BIGINT NOT NULL,
    usage_event_id TEXT NOT NULL,
    old_millicents BIGINT NOT NULL,
    new_millicents BIGINT NOT NULL,
    old_cost_basis TEXT NOT NULL,
    new_cost_basis TEXT NOT NULL,
    old_cost_cents NUMERIC NOT NULL,
    old_subscription_rate_card_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(period_id,revision,usage_event_id)
);

CREATE TABLE subscription_quota_components (
    period_id BIGINT NOT NULL REFERENCES subscription_billing_periods(id),
    counter_key TEXT NOT NULL,
    scope_type TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    limit_type TEXT NOT NULL,
    amount_millicents BIGINT NOT NULL CHECK(amount_millicents>=0),
    PRIMARY KEY(period_id,counter_key)
);

ALTER TABLE usage_events ADD COLUMN subscription_billing_period_id BIGINT REFERENCES subscription_billing_periods(id);
ALTER TABLE usage_events DROP CONSTRAINT usage_events_cost_basis_check;
ALTER TABLE usage_events ADD CONSTRAINT usage_events_cost_basis_check CHECK (cost_basis IN ('metered','subscription_rate_card','default_model_rate','subscription_pending','subscription_observed_delta'));
CREATE INDEX usage_events_subscription_completion_idx ON usage_events(provider_account_id,completed_at);
ALTER TABLE quota_spend_settlements DROP CONSTRAINT quota_spend_settlements_operation_check;
ALTER TABLE quota_spend_settlements ADD CONSTRAINT quota_spend_settlements_operation_check CHECK (operation IN ('add','ensure_minimum','reconcile_component'));
ALTER TABLE quota_spend_settlements DROP CONSTRAINT quota_spend_settlements_amount_millicents_check;
ALTER TABLE quota_spend_settlements ADD CONSTRAINT quota_spend_settlements_amount_millicents_check CHECK (amount_millicents >= 0);
ALTER TABLE quota_spend_settlements ADD COLUMN component_key TEXT NOT NULL DEFAULT '', ADD COLUMN component_version BIGINT NOT NULL DEFAULT 0;

-- Protect the ledger against stale provider caches, duplicate recorder delivery
-- and other cost writers. Only the transactional allocator can set final costs.
CREATE FUNCTION protect_observed_subscription_cost() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF current_setting('piproxy.subscription_allocator',true) = 'on' THEN RETURN NEW; END IF;
    IF EXISTS(SELECT 1 FROM provider_accounts WHERE id=NEW.provider_account_id
       AND config->'billing'->>'allocation_method'='observed_delta') THEN
        IF TG_OP='UPDATE' AND OLD.subscription_billing_period_id IS NOT NULL THEN
            NEW.cost_total_cents := OLD.cost_total_cents;
            NEW.cost_basis := OLD.cost_basis;
            NEW.subscription_billing_period_id := OLD.subscription_billing_period_id;
        ELSE
            NEW.cost_total_cents := 0;
            NEW.cost_basis := 'subscription_pending';
            NEW.subscription_billing_period_id := NULL;
        END IF;
        NEW.subscription_rate_card_id := NULL;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER usage_observed_subscription_cost BEFORE INSERT OR UPDATE ON usage_events
    FOR EACH ROW EXECUTE FUNCTION protect_observed_subscription_cost();

INSERT INTO schema_migrations(version,applied_at) VALUES(24,now()) ON CONFLICT(version) DO NOTHING;
