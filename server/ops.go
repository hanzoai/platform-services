package server

// Operation ordinals — the per-category procedure selector carried in
// PSRequest.Op. Each block mirrors exactly one migrated tRPC router's procedure
// set, in source order. The ZAP method (wire.go Method*) selects the CATEGORY
// (and its permission bit); Op selects the OPERATION within it. This keeps
// "which capability" (method/permission) orthogonal to "which procedure" (Op).
//
// The console's TS client emits the SAME ordinals (the .zap file is the shared
// contract); both sides MUST keep these blocks in lockstep with the routers.

// cloudStatusRouter — MethodCloudStatus.
const (
	OpCloudGetStatus uint32 = iota // cloudStatus.getStatus
)

// platformRouter — MethodPlatform.
const (
	OpPlatformListContainers uint32 = iota // platform.listContainers
	OpPlatformGetContainer                 // platform.getContainer
	OpPlatformListPipelines                // platform.listPipelines
	OpPlatformTriggerBuild                 // platform.triggerBuild
)

// infrastructureRouter — MethodInfra.
const (
	OpInfraServiceHealth    uint32 = iota // infrastructure.getServiceHealth
	OpInfraDeploymentEvents               // infrastructure.getDeploymentEvents
)

// explorerRouter — MethodExplorer.
const (
	OpExplorerStats    uint32 = iota // explorer.getStats
	OpExplorerAllStats               // explorer.getAllStats
	OpExplorerHealth                 // explorer.getHealth
)

// searchRouter — MethodSearch.
const (
	OpSearchStats         uint32 = iota // search.stats
	OpSearchListIndexes                 // search.listIndexes
	OpSearchCreateIndex                 // search.createIndex
	OpSearchDeleteIndex                 // search.deleteIndex
	OpSearchReindex                     // search.reindex
	OpSearchQuery                       // search.query
	OpSearchChat                        // search.chat
	OpSearchGetKeys                     // search.getKeys
	OpSearchRegenerateKey               // search.regenerateKey
	OpSearchScrapePreview               // search.scrapePreview
)

// vectorRouter — MethodVector.
const (
	OpVectorStats            uint32 = iota // vector.stats
	OpVectorListCollections                // vector.listCollections
	OpVectorCreateCollection               // vector.createCollection
	OpVectorDeleteCollection               // vector.deleteCollection
	OpVectorSearch                         // vector.search
)

// naturalLanguageFilterRouter — MethodNLFilter.
const (
	OpNLFilterCreateCompletion uint32 = iota // naturalLanguageFilter.createCompletion
)

// botRouter — MethodBot.
const (
	OpBotList                uint32 = iota // bot.list
	OpBotGetByID                           // bot.getById
	OpBotCreate                            // bot.create
	OpBotUpdate                            // bot.update
	OpBotDelete                            // bot.delete
	OpBotStart                             // bot.start (billing-gated upstream)
	OpBotStop                              // bot.stop
	OpBotRestart                           // bot.restart (billing-gated upstream)
	OpBotGetUsage                          // bot.getUsage
	OpBotGetLogs                           // bot.getLogs
	OpBotGetBilling                        // bot.getBilling
	OpBotUpgradePlan                       // bot.upgradePlan
	OpBotListPaymentMethods                // bot.listPaymentMethods
	OpBotAddPaymentMethod                  // bot.addPaymentMethod
	OpBotGetCredits                        // bot.getCredits
	OpBotGetBalance                        // bot.getBalance
	OpBotListSecrets                       // bot.listSecrets (KMS-backed)
	OpBotListTeamPresets                   // bot.listTeamPresets
	OpBotGetTeamPreset                     // bot.getTeamPreset
	OpBotProvisionTeamPreset               // bot.provisionTeamPreset
	OpBotProvisionAllPresets               // bot.provisionAllTeamPresets
	OpBotGetAgentDID                       // bot.getAgentDID
	OpBotCreateAgentDID                    // bot.createAgentDID
	OpBotGetAgentWallet                    // bot.getAgentWallet
	OpBotCreateAgentWallet                 // bot.createAgentWallet
	OpBotGetAgentIdentity                  // bot.getAgentIdentity
)

// kmsRouter — MethodKMS.
const (
	OpKMSListSecrets      uint32 = iota // kms.listSecrets
	OpKMSCreateSecret                   // kms.createSecret
	OpKMSUpdateSecret                   // kms.updateSecret
	OpKMSDeleteSecret                   // kms.deleteSecret
	OpKMSListEnvironments               // kms.listEnvironments
	OpKMSListKeys                       // kms.listKeys
	OpKMSCreateKey                      // kms.createKey
	OpKMSUpdateKey                      // kms.updateKey
	OpKMSDeleteKey                      // kms.deleteKey
	OpKMSEncrypt                        // kms.encrypt
	OpKMSDecrypt                        // kms.decrypt
)

// cloudBillingRouter — MethodBilling (EE).
const (
	OpBillingGetSubscriptionInfo uint32 = iota // cloudBilling.getSubscriptionInfo
	OpBillingCreateCheckout                    // cloudBilling.createStripeCheckoutSession
	OpBillingChangeProduct                     // cloudBilling.changeStripeSubscriptionProduct
	OpBillingCancel                            // cloudBilling.cancelStripeSubscription
	OpBillingReactivate                        // cloudBilling.reactivateStripeSubscription
	OpBillingClearSwitch                       // cloudBilling.clearPlanSwitchSchedule
	OpBillingCustomerPortal                    // cloudBilling.getStripeCustomerPortalUrl
	OpBillingGetInvoices                       // cloudBilling.getInvoices
	OpBillingGetUsage                          // cloudBilling.getUsage
	OpBillingApplyPromotion                    // cloudBilling.applyPromotionCode
)

// spendAlertRouter — MethodSpendAlert (EE). The ONLY category stored in Base.
const (
	OpSpendAlertList   uint32 = iota // spendAlert.getSpendAlerts
	OpSpendAlertCreate               // spendAlert.createSpendAlert
	OpSpendAlertUpdate               // spendAlert.updateSpendAlert
	OpSpendAlertDelete               // spendAlert.deleteSpendAlert
)
