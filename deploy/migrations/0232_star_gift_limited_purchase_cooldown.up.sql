-- Anti-scalper cooldown for LIMITED ("drop") star gifts: bot/scoop scripts
-- were buying up an entire limited drop's supply in seconds. One row per
-- buyer holding only their most recent limited-gift purchase timestamp --
-- enforced with an atomic UPSERT guarded by a WHERE clause in
-- prepareStarGiftPurchase, so concurrent purchase attempts from the same
-- user can't race past the cooldown. Not used for unlimited/regular gifts.
CREATE TABLE public.star_gift_limited_purchase_cooldowns (
    user_id bigint NOT NULL,
    last_purchase_date integer NOT NULL,
    CONSTRAINT star_gift_limited_purchase_cooldowns_pkey PRIMARY KEY (user_id),
    CONSTRAINT star_gift_limited_purchase_cooldowns_shape_check CHECK (user_id > 0 AND last_purchase_date > 0)
);
