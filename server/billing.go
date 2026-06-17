package server

import (
	"context"
	"fmt"
)

// Billing category (EE) — port of the console cloudBillingRouter. The router
// delegated to stripeBillingService.ts, a 65KB server module that orchestrates
// Stripe via its server SDK: checkout sessions, plan changes with proration,
// scheduled downgrades, customer-portal links, invoice pagination, metered
// usage, promotion codes. That orchestration logic is NOT a thin REST mapping —
// it composes many Stripe calls with branch-heavy billing rules (immediate vs
// prorated upgrade, period-end downgrade, legacy metered transition).
//
// Reimplementing that surface against Stripe's raw REST API in this binary would
// be a large, error-prone port of business logic that must stay byte-identical
// to the console's. It is therefore deferred behind a single explicit blocker:
// the Stripe billing orchestration should be extracted into a shared Go billing
// package (or a Stripe-facing microservice) that BOTH this service and the
// console call, rather than duplicated. Until then every billing op returns a
// precondition error naming the blocker.
//
// TODO(billing): wire Stripe billing orchestration. Requires STRIPE_SECRET_KEY
// (+ NEXTAUTH_URL for checkout/portal return_url) and a Go port of
// stripeBillingService (getSubscriptionInfo, createCheckoutSession, changePlan,
// cancel, reactivate, clearPlanSwitchSchedule, getCustomerPortalUrl,
// getInvoices, getUsage, applyPromotionCode). Extract to a shared billing pkg;
// do not duplicate the proration/downgrade rules here. getUsage additionally
// reads local Project counts (projectCount, projectsCreatedThisMonth) — those
// come from the org's Base/console project store, not Stripe.
func (s *Server) handleBilling(_ context.Context, _ string, req psRequest) (uint32, []byte) {
	switch req.Op {
	case OpBillingGetSubscriptionInfo, OpBillingCreateCheckout, OpBillingChangeProduct,
		OpBillingCancel, OpBillingReactivate, OpBillingClearSwitch, OpBillingCustomerPortal,
		OpBillingGetInvoices, OpBillingGetUsage, OpBillingApplyPromotion:
		if s.up.StripeSecretKey == "" {
			return fail(StatusPrecondition, "billing: STRIPE_SECRET_KEY not configured")
		}
		return fail(StatusInternal, "billing: Stripe orchestration not yet wired (see TODO in billing.go — extract shared Go billing pkg)")
	default:
		return fail(StatusBadRequest, fmt.Sprintf("billing: unknown op %d", req.Op))
	}
}
