package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

func openCodeGoCompleteSSRFixture(currentWorkspaceID string, subscriptionReference string) string {
	return fmt.Sprintf(`
<!doctype html>
	<html>
	  <head><link href="/_build/assets/go-route.js" rel="modulepreload"></head>
	  <body>
    <form action="/_server?id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa">
      <input value="false" name="useChinaProviders">
    </form>
    <script>
      $R[1] = [
        { name: "Primary", id: "wrk_ALPHA1" },
        { id: "wrk_BETA2", name: "Secondary" }
      ];
      $R[2]($R[3], "operator@example.test");
      $R[4] = {
        workspaceID: "%s",
        liteSubscriptionID: "%s",
        referralCode: "SAMPLECODE",
        rollingUsage: $R[91] = {
          usagePercent: 101.25,
          status: "exhausted",
          resetInSec: 3600
        },
        weeklyUsage: $R[7] = { resetInSec: 7200, usagePercent: 22.5, status: "active" },
        monthlyUsage: $R[123] = { status: "active", usagePercent: 33, resetInSec: 10800 }
      };
      $R[5] = [
        { source: "inviter", status: "available", id: "ref_SAMPLE1" },
        { id: "ref_SAMPLE2", source: "invitee", status: "used" },
        { id: "ref_SAMPLE2", source: "invitee", status: "applied" }
      ];
    </script>
  </body>
</html>`, currentWorkspaceID, subscriptionReference)
}

func TestParseOpenCodeGoConsolePageCompleteSnapshot(t *testing.T) {
	fetchedAt := time.Unix(1_900_000_000, 0)
	page, err := ParseOpenCodeGoConsolePage(
		openCodeGoCompleteSSRFixture("wrk_BETA2", "sub_SAMPLE2"),
		"wrk_BETA2",
		fetchedAt,
	)
	require.NoError(t, err)
	require.Equal(t, "wrk_BETA2", page.WorkspaceID)
	require.Equal(t, "Secondary", page.WorkspaceName)
	require.Equal(t, "operator@example.test", page.Email)
	require.Equal(t, model.OpenCodeGoMembershipActive, page.MembershipStatus)
	require.Equal(t, "sub_SAMPLE2", page.SubscriptionReference)
	require.Equal(t, "SAMPLECODE", page.ReferralCode)
	require.Equal(t, 1, page.AvailableReferralRewards)
	require.Equal(t, 1, page.UsedReferralRewards)
	require.Equal(t, []string{"ref_SAMPLE1"}, page.AvailableReferralRewardIDs)
	require.Equal(t, []string{"ref_SAMPLE2"}, page.UsedReferralRewardIDs)
	require.NotNil(t, page.ChinaModelsEnabled)
	require.False(t, *page.ChinaModelsEnabled)
	require.Equal(t, strings.Repeat("a", 64), page.ChinaModelsServerID)
	require.Equal(t, []string{"/_build/assets/go-route.js"}, page.RouteModuleAssets)
	require.Equal(t, []OpenCodeGoDiscoveredWorkspace{
		{ID: "wrk_ALPHA1", Name: "Primary"},
		{ID: "wrk_BETA2", Name: "Secondary"},
	}, page.Workspaces)

	require.Empty(t, page.QuotaParseError)
	require.NotNil(t, page.Quota)
	require.Equal(t, fetchedAt.Unix(), page.Quota.FetchedAt)
	require.Equal(t, fetchedAt.Unix()+3600, page.Quota.NextRefreshAt)
	require.Equal(t, []OpenCodeGoAuthoritativeQuotaWindow{
		{Kind: model.OpenCodeGoQuotaRolling, UsedPercent: 101.25, ResetSeconds: 3600, ResetAt: fetchedAt.Unix() + 3600, FetchedAt: fetchedAt.Unix()},
		{Kind: model.OpenCodeGoQuotaWeekly, UsedPercent: 22.5, ResetSeconds: 7200, ResetAt: fetchedAt.Unix() + 7200, FetchedAt: fetchedAt.Unix()},
		{Kind: model.OpenCodeGoQuotaMonthly, UsedPercent: 33, ResetSeconds: 10800, ResetAt: fetchedAt.Unix() + 10800, FetchedAt: fetchedAt.Unix()},
	}, page.Quota.Windows)
}

func TestParseOpenCodeGoConsolePageRejectsEveryIncompleteQuotaPair(t *testing.T) {
	fixture := openCodeGoCompleteSSRFixture("wrk_ALPHA1", "sub_SAMPLE1")
	tests := map[string][2]string{
		"rolling usage": {"usagePercent: 101.25", "usageValue: 101.25"},
		"rolling reset": {"resetInSec: 3600", "resetValue: 3600"},
		"weekly usage":  {"usagePercent: 22.5", "usageValue: 22.5"},
		"weekly reset":  {"resetInSec: 7200", "resetValue: 7200"},
		"monthly usage": {"usagePercent: 33", "usageValue: 33"},
		"monthly reset": {"resetInSec: 10800", "resetValue: 10800"},
	}

	for name, replacement := range tests {
		t.Run(name, func(t *testing.T) {
			page, err := ParseOpenCodeGoConsolePage(
				strings.Replace(fixture, replacement[0], replacement[1], 1),
				"wrk_ALPHA1",
				time.Unix(1_900_000_000, 0),
			)
			require.NoError(t, err)
			require.Nil(t, page.Quota)
			require.NotEmpty(t, page.QuotaParseError)
		})
	}
}

func TestParseOpenCodeGoConsolePageRejectsInvalidQuotaNumbers(t *testing.T) {
	fixture := openCodeGoCompleteSSRFixture("wrk_ALPHA1", "sub_SAMPLE1")
	tests := map[string]string{
		"negative usage":   strings.Replace(fixture, "usagePercent: 22.5", "usagePercent: -1", 1),
		"fractional reset": strings.Replace(fixture, "resetInSec: 7200", "resetInSec: 1.5", 1),
		"distant reset":    strings.Replace(fixture, "resetInSec: 10800", "resetInSec: 999999999", 1),
		"infinite usage":   strings.Replace(fixture, "usagePercent: 33", "usagePercent: 1e999", 1),
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			page, err := ParseOpenCodeGoConsolePage(document, "wrk_ALPHA1", time.Unix(1_900_000_000, 0))
			require.NoError(t, err)
			require.Nil(t, page.Quota)
			require.NotEmpty(t, page.QuotaParseError)
		})
	}
}

func TestParseOpenCodeGoConsolePageRequiresExplicitMembership(t *testing.T) {
	fixture := openCodeGoCompleteSSRFixture("wrk_ALPHA1", "sub_SAMPLE1")
	unknown := strings.Replace(fixture, `liteSubscriptionID: "sub_SAMPLE1",`, `unrelatedProperty: "value",`, 1)
	page, err := ParseOpenCodeGoConsolePage(unknown, "wrk_ALPHA1", time.Now())
	require.NoError(t, err)
	require.Equal(t, model.OpenCodeGoMembershipUnknown, page.MembershipStatus)
	require.Nil(t, page.Quota)
	require.NotEmpty(t, page.QuotaParseError)

	inactive := strings.Replace(fixture, `liteSubscriptionID: "sub_SAMPLE1",`, `liteSubscriptionID: null,`, 1)
	page, err = ParseOpenCodeGoConsolePage(inactive, "wrk_ALPHA1", time.Now())
	require.NoError(t, err)
	require.Equal(t, model.OpenCodeGoMembershipInactive, page.MembershipStatus)
	require.Nil(t, page.Quota)
	require.Empty(t, page.QuotaParseError)
}

func TestParseOpenCodeGoAPIKeyPage(t *testing.T) {
	key, err := ParseOpenCodeGoAPIKeyPage(`<html><script>$R[1] = { label: "Primary", key: "sk-synthetic-value" };</script></html>`)
	require.NoError(t, err)
	require.Equal(t, "sk-synthetic-value", key)

	key, err = ParseOpenCodeGoAPIKeyPage(`<html><script>$R[1] = { key: "masked-value" };</script></html>`)
	require.NoError(t, err)
	require.Empty(t, key)
}
