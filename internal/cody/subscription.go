package cody

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/featureflag"
	"github.com/sourcegraph/sourcegraph/internal/ssc"
	"github.com/sourcegraph/sourcegraph/internal/types"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

// featureFlagUseSCCForSubscription determines if we should attempt to lookup subscription data from SSC.
const featureFlagUseSCCForSubscription = "use-ssc-for-cody-subscription"

// featureFlagCodyProTrialEnded indicates if the Cody Pro "Free Trial"  has ended.
// (Enabling users to use Cody Pro for free for 3-months starting in late Q4'2023.)
const featureFlagCodyProTrialEnded = "cody-pro-trial-ended"

type UserSubscriptionPlan string

const (
	UserSubscriptionPlanFree UserSubscriptionPlan = "FREE"
	UserSubscriptionPlanPro  UserSubscriptionPlan = "PRO"
)

type UserSubscription struct {
	// Status is the current status of the subscription. "pending" means the user has no Cody Pro subscription.
	// "pending" subscription will be removed post Feb 15, 2024. It is required to support users who have opted for
	// Cody Pro Trial on dotcom, but have not entered payment information on SSC.
	// (So they don't have an SSC backed subscription, but we need to act like they do, until 2/15.)
	Status ssc.SubscriptionStatus
	// Plan is the plan the user is subscribed to.
	Plan UserSubscriptionPlan
	// ApplyProRateLimits indicates the user should be given higher rate limits
	// for Cody and related functionality. (Use this value instead of checking
	// the subscription status for simplicity.)
	ApplyProRateLimits bool
	// CurrentPeriodStartAt is the start date of the current billing cycle.
	CurrentPeriodStartAt time.Time
	// CurrentPeriodEndAt is the end date of the current billing cycle.
	// IMPORTANT: This may be IN THE PAST. e.g. if the subscription was
	// canceled, this will be when the subscription ended.
	CurrentPeriodEndAt time.Time
}

// consolidateSubscriptionDetails merges the subscription data available on dotcom and SCC.
// This is needed while transitioning to use SCC as the source of truth for all subscription data. (Which should happen ~Q1/2024.)
// TODO[sourcegraph#59785]: Update dotcom to use SSC as the source of truth for all subscription data.
func consolidateSubscriptionDetails(ctx context.Context, user types.User, subscription *ssc.Subscription) (*UserSubscription, error) {
	// If subscription information is available from SSC, we use that.
	// And just ignore what is stored in dotcom. (Since they've already
	// been migrated so to speak.)
	if subscription != nil {
		currentPeriodStart, err := time.Parse(time.RFC3339, subscription.CurrentPeriodStart)
		if err != nil {
			return nil, err
		}

		currentPeriodEnd, err := time.Parse(time.RFC3339, subscription.CurrentPeriodEnd)
		if err != nil {
			return nil, err
		}

		applyProRateLimits := subscription.Status == ssc.SubscriptionStatusActive || subscription.Status == ssc.SubscriptionStatusPastDue || subscription.Status == ssc.SubscriptionStatusTrialing

		return &UserSubscription{
			Status:               subscription.Status,
			Plan:                 UserSubscriptionPlanPro,
			ApplyProRateLimits:   applyProRateLimits,
			CurrentPeriodStartAt: currentPeriodStart,
			CurrentPeriodEndAt:   currentPeriodEnd,
		}, nil
	}

	// If the user doesn't have a subscription in the SSC backend, then we need
	// synthesize one using the data available on dotcom.
	currentPeriodStartAt, currentPeriodEndAt := preSSCReleaseCurrentPeriodDateRange(ctx, user)

	// Whether or not the Cody Pro free trial offer is still running.
	codyProTrialEnded := featureflag.FromContext(ctx).GetBoolOr(featureFlagCodyProTrialEnded, false)

	if user.CodyProEnabledAt != nil {
		return &UserSubscription{
			Status:               ssc.SubscriptionStatusPending,
			Plan:                 UserSubscriptionPlanPro,
			ApplyProRateLimits:   !codyProTrialEnded,
			CurrentPeriodStartAt: currentPeriodStartAt,
			CurrentPeriodEndAt:   currentPeriodEndAt,
		}, nil
	}

	return &UserSubscription{
		Status:               ssc.SubscriptionStatusPending,
		Plan:                 UserSubscriptionPlanFree,
		ApplyProRateLimits:   false,
		CurrentPeriodStartAt: currentPeriodStartAt,
		CurrentPeriodEndAt:   currentPeriodEndAt,
	}, nil
}

// getSAMSAccountIDForUser returns the user's SAMS account ID if available.
//
// If the user has not associated a SAMS identity with their dotcom user account,
// will return ("", nil). After we migrate all dotcom user accounts to SAMS, that
// should no longer be possible.
func getSAMSAccountIDForUser(ctx context.Context, db database.DB, dotcomUserID int32) (string, error) {
	// NOTE: We hard-code this to look for the SAMS-prod environment, meaning there isn't a way
	// to test dotcom pulling subscription data from a local SAMS/SSC instance. To support that
	// we'd need to make the SAMSHostname configurable. (Or somehow identify which OIDC provider
	// is SAMS.)
	oidcAccounts, err := db.UserExternalAccounts().List(ctx, database.ExternalAccountsListOptions{
		UserID:      dotcomUserID,
		ServiceType: "openidconnect",
		ServiceID:   fmt.Sprintf("https://%s", ssc.SAMSProdHostname),
		LimitOffset: &database.LimitOffset{
			Limit: 1,
		},
	})
	if err != nil {
		return "", errors.Wrap(err, "listing external accounts")
	}

	if len(oidcAccounts) > 0 {
		return oidcAccounts[0].AccountID, nil
	}
	return "", nil
}

// Returns the subscription data for the given user. (And reconciling the data between both
// the dotcom database and the SSC backend.
func getSubscriptionForUser(ctx context.Context, db database.DB, sscClient ssc.Client, user types.User) (*UserSubscription, error) {
	samsAccountID, err := getSAMSAccountIDForUser(ctx, db, user.ID)
	if err != nil {
		return nil, errors.Wrap(err, "fetching user's SAMS account ID")
	}

	// While developing the SSC backend, we only fetch subscription data for users based on a flag.
	var subscription *ssc.Subscription
	useSCCForSubscriptionData := featureflag.FromContext(ctx).GetBoolOr(featureFlagUseSCCForSubscription, false)
	if samsAccountID != "" && useSCCForSubscriptionData {
		subscription, err = sscClient.FetchSubscriptionBySAMSAccountID(ctx, samsAccountID)
		if err != nil {
			return nil, errors.Wrap(err, "fetching subscription from SSC")
		}
	}

	return consolidateSubscriptionDetails(ctx, user, subscription)
}

// SubscriptionForUser returns the user's Cody subscription details.
func SubscriptionForUser(ctx context.Context, db database.DB, user types.User) (*UserSubscription, error) {
	sscClient := getSSCClient()
	return getSubscriptionForUser(ctx, db, sscClient, user)
}

// getSSCClient returns a self-service Cody API client. We only do this once so that the stateless client
// can persist in memory longer, so we can benefit from the underlying HTTP client only needing to reissue
// SAMS access tokens when needed, rather than minting a new token for every request.
//
// BUG: If the SAMS configuration is added or changed during the lifetime of the this process, the returned
// client will be invalid. (As it would be using the original SAMS client configuration data.) The process
// will need to be restarted to correct this situation.
var getSSCClient = sync.OnceValue[ssc.Client](func() ssc.Client {
	return ssc.NewClient()
})
