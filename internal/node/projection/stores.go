package projection

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	privacyapp "telesrv/internal/app/privacy"
	"telesrv/internal/app/userprojection"
	storepkg "telesrv/internal/store"
	"telesrv/internal/store/postgres"
	"telesrv/internal/store/redisstore"
)

type StoreConfig struct {
	ChannelRowCacheMaxEntries    int
	ChannelMemberCacheMaxEntries int
	ChannelDialogCacheMaxEntries int
	ChannelBoostCacheMaxEntries  int
	ChannelBoostCacheTTL         time.Duration
	// StarGiftTONStartingGrant only matters to the process that actually calls
	// CollectiblePhoneStore.PurchaseCollectiblePhoneWithDelivery (Core); Egress
	// leaves it zero since it never performs a purchase.
	StarGiftTONStartingGrant int64
}

type StoreOptions struct {
	ChannelOptions []postgres.ChannelStoreOption
	MessageOptions []postgres.MessageStoreOption
}

// Stores are the shared projection read-model primitives used by Core and
// Egress. Each process owns its own cache instances; the construction rules stay
// centralized so viewer-specific projections cannot drift between roles.
type Stores struct {
	UserStore             *postgres.UserStore
	ContactStore          *userprojection.CachedContactStore
	CollectiblePhoneStore *postgres.CollectiblePhoneStore
	ReadModelVersions     *storepkg.CachedReadModelVersionStore
	DialogStore           *postgres.DialogStore
	MessageStore          *postgres.MessageStore
	ChannelStore          *postgres.ChannelStore
	MediaStore            *postgres.MediaStore
	Photos                *userprojection.CachedPhotoProvider
	PrivacyStore          *privacyapp.CachedPrivacyStore
	UserCache             *redisstore.UserCache
	ChannelRowCache       *postgres.ChannelRowCache
	ChannelMemberCache    *postgres.ChannelMemberCache
	ChannelDialogCache    *postgres.ChannelDialogCache
	ChannelBoostCache     *postgres.ChannelBoostCache
}

func NewStores(pool *pgxpool.Pool, rdb *redis.Client, cfg StoreConfig, opts StoreOptions, logger *zap.Logger) Stores {
	if logger == nil {
		logger = zap.NewNop()
	}
	contactStore := userprojection.NewCachedContactStore(postgres.NewContactStore(pool), 0)
	mediaStore := postgres.NewMediaStore(pool)
	channelRowCache := postgres.NewChannelRowCache(cfg.ChannelRowCacheMaxEntries)
	channelMemberCache := postgres.NewChannelMemberCache(cfg.ChannelMemberCacheMaxEntries)
	channelDialogCache := postgres.NewChannelDialogCache(cfg.ChannelDialogCacheMaxEntries)
	channelBoostCache := postgres.NewChannelBoostCache(cfg.ChannelBoostCacheMaxEntries, cfg.ChannelBoostCacheTTL)
	channelOptions := []postgres.ChannelStoreOption{
		postgres.WithChannelLogger(logger.Named("store").Named("channels")),
		postgres.WithChannelRowCache(channelRowCache),
		postgres.WithChannelMemberCache(channelMemberCache),
		postgres.WithChannelDialogCache(channelDialogCache),
		postgres.WithChannelBoostCache(channelBoostCache),
	}
	channelOptions = append(channelOptions, opts.ChannelOptions...)
	messageOptions := []postgres.MessageStoreOption{
		postgres.WithMessageLogger(logger.Named("store").Named("messages")),
	}
	messageOptions = append(messageOptions, opts.MessageOptions...)

	return Stores{
		UserStore:             postgres.NewUserStore(pool),
		ContactStore:          contactStore,
		CollectiblePhoneStore: postgres.NewCollectiblePhoneStore(pool, postgres.WithCollectiblePhoneTONStartingGrant(cfg.StarGiftTONStartingGrant)),
		ReadModelVersions:     storepkg.NewCachedReadModelVersionStore(postgres.NewReadModelVersionStore(pool), 0, 0),
		DialogStore:           postgres.NewDialogStore(pool),
		MessageStore:          postgres.NewMessageStore(pool, messageOptions...),
		ChannelStore:          postgres.NewChannelStore(pool, channelOptions...),
		MediaStore:            mediaStore,
		Photos:                userprojection.NewCachedPhotoProvider(mediaStore, userprojection.DefaultPhotoCacheTTL),
		PrivacyStore:          privacyapp.NewCachedPrivacyStore(postgres.NewPrivacyStore(pool), 0),
		UserCache:             redisstore.NewUserCache(rdb, redisstore.DefaultUserCacheTTL),
		ChannelRowCache:       channelRowCache,
		ChannelMemberCache:    channelMemberCache,
		ChannelDialogCache:    channelDialogCache,
		ChannelBoostCache:     channelBoostCache,
	}
}

func (s Stores) NewPrivacyService() *privacyapp.Service {
	return privacyapp.NewService(s.PrivacyStore, s.ContactStore)
}

type CacheSetDeps struct {
	ContactExtras       []postgres.ContactReadModelCache
	Dialogs             postgres.DialogReadModelCache
	Privacy             postgres.PrivacyReadModelCache
	Stories             postgres.StoryReadModelCache
	ChannelFullBots     postgres.ChannelFullBotReadModelCache
	ChannelBotMembers   postgres.ChannelBotMemberReadModelCache
	ChannelMediaCounts  postgres.ChannelMediaCountReadModelCache
	PrivateMediaCounts  postgres.PrivateMediaCountReadModelCache
	RPCProjections      postgres.RPCProjectionReadModelCache
	PeerIdentities      postgres.PeerIdentityReadModelCache
	BotProfiles         postgres.BotProfileReadModelCache
	StarGifts           postgres.StarGiftCatalogCache
	AccountSettings     postgres.AccountSettingsReadModelCache
	AccountFreezes      postgres.AccountFreezeReadModelCache
	UserProjectionFacts postgres.UserProjectionFactReadModelCache
	BusinessAutomation  postgres.BusinessAutomationReadModelCache
}

func (s Stores) ReadModelCacheSet(deps CacheSetDeps) postgres.ReadModelCacheSet {
	return postgres.ReadModelCacheSet{
		ReadModelVersions:   s.ReadModelVersions,
		ChannelRows:         s.ChannelRowCache,
		ChannelMembers:      s.ChannelMemberCache,
		ChannelDialogs:      s.ChannelDialogCache,
		ChannelBoosts:       s.ChannelBoostCache,
		Contacts:            mergeContactCaches(s.ContactStore, deps.ContactExtras...),
		Dialogs:             deps.Dialogs,
		Privacy:             deps.Privacy,
		ProfilePhotos:       s.Photos,
		Stories:             deps.Stories,
		ChannelFullBots:     deps.ChannelFullBots,
		ChannelBotMembers:   deps.ChannelBotMembers,
		ChannelMediaCounts:  deps.ChannelMediaCounts,
		PrivateMediaCounts:  deps.PrivateMediaCounts,
		RPCProjections:      deps.RPCProjections,
		PeerIdentities:      deps.PeerIdentities,
		BaseUsers:           s.UserCache,
		BotProfiles:         deps.BotProfiles,
		StarGifts:           deps.StarGifts,
		AccountSettings:     deps.AccountSettings,
		AccountFreezes:      deps.AccountFreezes,
		UserProjectionFacts: deps.UserProjectionFacts,
		CollectiblePhones:   s.CollectiblePhoneStore,
		BusinessAutomation:  deps.BusinessAutomation,
	}
}

func mergeContactCaches(base postgres.ContactReadModelCache, extras ...postgres.ContactReadModelCache) postgres.ContactReadModelCache {
	caches := make([]postgres.ContactReadModelCache, 0, 1+len(extras))
	if base != nil {
		caches = append(caches, base)
	}
	for _, extra := range extras {
		if extra != nil {
			caches = append(caches, extra)
		}
	}
	switch len(caches) {
	case 0:
		return nil
	case 1:
		return caches[0]
	default:
		return postgres.ContactReadModelCaches(caches)
	}
}
