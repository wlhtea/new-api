package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const openCodeGoMaxImportCookies = 100

// openCodeGoImportAutomationTimeout bounds the background lifecycle
// automation started right after an identity import.
const openCodeGoImportAutomationTimeout = 5 * time.Minute

var (
	openCodeGoIdentityOperationLocks sync.Map
	openCodeGoSecretPattern          = regexp.MustCompile(`(?i)sk-[a-z0-9._-]+`)
	openCodeGoCookiePattern          = regexp.MustCompile(`(?i)auth=[^;\s]+`)
	openCodeGoUpstreamIDPattern      = regexp.MustCompile(`(?i)wrk_[a-z0-9]+`)
	openCodeGoReferralIDPattern      = regexp.MustCompile(`(?i)ref_[a-z0-9]+`)
	openCodeGoPortalSessionPattern   = regexp.MustCompile(`(?i)bps_[a-z0-9]+`)
	openCodeGoPortalPathPattern      = regexp.MustCompile(`(?i)(/p/session/)[^/?#\s]+`)
	openCodeGoIdentityUIDPattern     = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\b`)
	openCodeGoProxyUserinfoPattern   = regexp.MustCompile(`(?i)\b(?:https?|socks5h?)://[^/@\s]+@[^/\s]+`)
	openCodeGoAuthorizationPattern   = regexp.MustCompile(`(?i)(\b(?:proxy-)?authorization\s*[:=]\s*)[^\r\n]+`)
)

type openCodeGoConsoleReader interface {
	DiscoverWorkspacePages(ctx context.Context, authCookie string, cachedWorkspaceID string) ([]OpenCodeGoWorkspacePageResult, error)
	FetchAPIKey(ctx context.Context, authCookie string, workspaceID string) (string, error)
	FetchModels(ctx context.Context, apiKey string) ([]string, error)
}

type openCodeGoConsoleReaderFactory func(channelID int, identityUID string) (openCodeGoConsoleReader, error)

type OpenCodeGoAccountPoolService struct {
	console                   openCodeGoConsoleReader
	consoleFactory            openCodeGoConsoleReaderFactory
	provisionalConsoleFactory openCodeGoConsoleReaderFactory
	codec                     *OpenCodeGoCredentialCodec
	now                       func() time.Time
	rebuild                   func(int) error
}

func (service *OpenCodeGoAccountPoolService) rebuildPoolChannel(channelID int) error {
	if service != nil && service.rebuild != nil {
		return service.rebuild(channelID)
	}
	return ReconcileOpenCodeGoPoolChannel(channelID)
}

type OpenCodeGoImportResult struct {
	Index          int    `json:"index"`
	Status         string `json:"status"`
	IdentityUID    string `json:"-"`
	WorkspaceCount int    `json:"workspace_count,omitempty"`
	Error          string `json:"error,omitempty"`
}

type openCodeGoPreparedWorkspace struct {
	record            model.OpenCodeGoWorkspace
	windows           []model.OpenCodeGoQuotaWindow
	models            []string
	modelsFresh       bool
	healthObservation OpenCodeGoHealthObservation
}

type openCodeGoIdentityCredentialUpdate struct {
	ciphertext  string
	fingerprint string
}

func NewConfiguredOpenCodeGoAccountPoolService() (*OpenCodeGoAccountPoolService, error) {
	codec, err := NewConfiguredOpenCodeGoCredentialCodec()
	if err != nil {
		return nil, err
	}
	return &OpenCodeGoAccountPoolService{
		consoleFactory: func(channelID int, identityUID string) (openCodeGoConsoleReader, error) {
			return newOpenCodeGoIdentityConsoleClient(channelID, identityUID)
		},
		provisionalConsoleFactory: func(channelID int, identityUID string) (openCodeGoConsoleReader, error) {
			baseClient, err := getOpenCodeGoProvisionalIdentityHTTPClient(channelID, identityUID)
			if err != nil {
				return nil, err
			}
			return newOpenCodeGoConsoleClient(openCodeGoConsoleOrigin, openCodeGoInferenceOrigin, baseClient)
		},
		codec:   codec,
		now:     time.Now,
		rebuild: RebuildOpenCodeGoPoolChannel,
	}, nil
}

func newOpenCodeGoAccountPoolService(console openCodeGoConsoleReader, codec *OpenCodeGoCredentialCodec) *OpenCodeGoAccountPoolService {
	return &OpenCodeGoAccountPoolService{
		console: console,
		codec:   codec,
		now:     time.Now,
		rebuild: RebuildOpenCodeGoPoolChannel,
	}
}

func NewOpenCodeGoAccountPoolAdminService() *OpenCodeGoAccountPoolService {
	return &OpenCodeGoAccountPoolService{
		now:     time.Now,
		rebuild: RebuildOpenCodeGoPoolChannel,
	}
}

func (service *OpenCodeGoAccountPoolService) scopedForIdentity(channelID int, identityUID string) (*OpenCodeGoAccountPoolService, error) {
	return service.scopedForIdentityWithFactory(channelID, identityUID, service.consoleFactory)
}

func (service *OpenCodeGoAccountPoolService) scopedForProvisionalIdentity(channelID int, identityUID string) (*OpenCodeGoAccountPoolService, error) {
	factory := service.provisionalConsoleFactory
	if factory == nil {
		factory = service.consoleFactory
	}
	return service.scopedForIdentityWithFactory(channelID, identityUID, factory)
}

func (service *OpenCodeGoAccountPoolService) scopedForIdentityWithFactory(
	channelID int,
	identityUID string,
	factory openCodeGoConsoleReaderFactory,
) (*OpenCodeGoAccountPoolService, error) {
	if service == nil || service.codec == nil {
		return nil, errors.New("OpenCode Go account pool service is not configured")
	}
	if service.console != nil {
		return service, nil
	}
	if factory == nil {
		return nil, errors.New("OpenCode Go account pool service is not configured")
	}
	console, err := factory(channelID, identityUID)
	if err != nil {
		return nil, err
	}
	scoped := *service
	scoped.console = console
	scoped.consoleFactory = nil
	scoped.provisionalConsoleFactory = nil
	return &scoped, nil
}

func (service *OpenCodeGoAccountPoolService) ImportAuthCookies(
	ctx context.Context,
	channelID int,
	label string,
	input string,
) ([]OpenCodeGoImportResult, error) {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(label) > 128 {
		return nil, errors.New("OpenCode Go identity label is too long")
	}

	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	results := make([]OpenCodeGoImportResult, 0, len(lines))
	seen := make(map[string]struct{})
	importedIdentityUIDs := make([]string, 0, len(lines))
	nonEmptyCount := 0
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		nonEmptyCount++
		result := OpenCodeGoImportResult{Index: index + 1}
		if nonEmptyCount > openCodeGoMaxImportCookies {
			result.Status = "error"
			result.Error = "OpenCode Go Cookie import limit exceeded"
			results = append(results, result)
			continue
		}
		authCookie, err := NormalizeOpenCodeGoAuthCookie(line)
		if err != nil {
			result.Status = "error"
			result.Error = sanitizeOpenCodeGoError(err)
			results = append(results, result)
			continue
		}
		fingerprint, err := service.codec.Fingerprint(OpenCodeGoCredentialAuthCookie, authCookie)
		if err != nil {
			result.Status = "error"
			result.Error = sanitizeOpenCodeGoError(err)
			results = append(results, result)
			continue
		}
		if _, duplicate := seen[fingerprint]; duplicate {
			result.Status = "duplicate"
			result.Error = "duplicate OpenCode Go Cookie in import request"
			results = append(results, result)
			continue
		}
		seen[fingerprint] = struct{}{}

		identityUID, workspaceCount, err := service.importOneIdentity(
			ctx,
			channelID,
			strings.TrimSpace(label),
			authCookie,
			fingerprint,
		)
		if err != nil {
			result.Status = "error"
			result.Error = sanitizeOpenCodeGoError(err, authCookie)
		} else {
			result.Status = "imported"
			result.IdentityUID = identityUID
			result.WorkspaceCount = workspaceCount
			importedIdentityUIDs = append(importedIdentityUIDs, identityUID)
		}
		results = append(results, result)
	}
	if nonEmptyCount == 0 {
		return nil, errors.New("at least one OpenCode Go auth Cookie is required")
	}
	// Newly imported identities should pick up the channel's lifecycle policy
	// (China-deployed models, renewal cancellation, referral rewards) right
	// away instead of waiting for the periodic refresh. Best-effort and
	// asynchronous: failures are recorded as operations and do not fail the
	// import. Only runs when the global automation master switch is enabled.
	if len(importedIdentityUIDs) > 0 && openCodeGoLifecycleAutomationEnabled() {
		runOpenCodeGoImportAutomations(context.Background(), channelID, importedIdentityUIDs)
	}
	return results, nil
}

// runOpenCodeGoImportAutomations applies the channel lifecycle policy to
// freshly imported identities in the background. It is bounded by a timeout
// and never returns an error to the caller.
func runOpenCodeGoImportAutomations(ctx context.Context, channelID int, identityUIDs []string) {
	runOpenCodeGoImportAutomationsWithRunner(ctx, channelID, identityUIDs, runConfiguredOpenCodeGoImportAutomations)
}

func runOpenCodeGoImportAutomationsWithRunner(
	ctx context.Context,
	channelID int,
	identityUIDs []string,
	runner func(context.Context, int, []string) error,
) {
	go func() {
		runCtx, cancel := context.WithTimeout(ctx, openCodeGoImportAutomationTimeout)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				common.SysError(sanitizeOpenCodeGoError(fmt.Errorf("OpenCode Go import automation panic recovered: %v", r)))
			}
		}()
		if err := runner(runCtx, channelID, identityUIDs); err != nil {
			common.SysError(sanitizeOpenCodeGoError(err))
		}
	}()
}

func runConfiguredOpenCodeGoImportAutomations(ctx context.Context, channelID int, identityUIDs []string) error {
	lifecycle, err := NewConfiguredOpenCodeGoLifecycleService()
	if err != nil {
		return fmt.Errorf("failed to start OpenCode Go import automation: %w", err)
	}
	for _, identityUID := range identityUIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := lifecycle.RunIdentityAutomations(ctx, channelID, identityUID, "import"); err != nil {
			common.SysError(sanitizeOpenCodeGoError(fmt.Errorf("OpenCode Go import automation failed: %w", err)))
		}
	}
	return nil
}

func (service *OpenCodeGoAccountPoolService) importOneIdentity(
	ctx context.Context,
	channelID int,
	label string,
	authCookie string,
	fingerprint string,
) (string, int, error) {
	unlock := lockOpenCodeGoIdentityOperation(fmt.Sprintf("%d:fingerprint:%s", channelID, fingerprint))
	defer unlock()

	existing, err := model.GetOpenCodeGoIdentityByFingerprint(channelID, fingerprint)
	if err != nil {
		return "", 0, err
	}
	if existing != nil {
		return "", 0, errors.New("OpenCode Go Cookie is already imported in this channel")
	}

	identityUID := uuid.NewString()
	scoped, err := service.scopedForProvisionalIdentity(channelID, identityUID)
	if err != nil {
		return "", 0, err
	}
	service = scoped
	ciphertext, err := service.codec.Encrypt(OpenCodeGoCredentialAuthCookie, channelID, identityUID, authCookie)
	if err != nil {
		return "", 0, err
	}
	discovered, err := service.console.DiscoverWorkspacePages(ctx, authCookie, "")
	if err != nil {
		return "", 0, err
	}
	if len(discovered) == 0 {
		return "", 0, errors.New("OpenCode Go console returned no workspaces")
	}

	now := service.now().Unix()
	identity := &model.OpenCodeGoIdentity{
		UID:                   identityUID,
		ChannelID:             channelID,
		Label:                 label,
		AuthCookieCiphertext:  ciphertext,
		AuthCookieFingerprint: fingerprint,
		Status:                model.OpenCodeGoIdentityStatusActive,
		LastSyncedAt:          now,
	}
	prepared := make([]openCodeGoPreparedWorkspace, 0, len(discovered))
	for _, result := range discovered {
		workspace, prepareErr := service.prepareWorkspace(ctx, channelID, authCookie, result, nil)
		if prepareErr != nil {
			return "", 0, prepareErr
		}
		if identity.Email == "" && workspace.record.Email != "" {
			identity.Email = workspace.record.Email
		}
		prepared = append(prepared, workspace)
	}

	err = model.DB.Transaction(func(tx *gorm.DB) error {
		if err := validateOpenCodeGoPoolChannelTx(tx, channelID); err != nil {
			return err
		}
		if err := tx.Create(identity).Error; err != nil {
			return err
		}
		for index := range prepared {
			prepared[index].record.IdentityID = identity.ID
			if err := createOpenCodeGoPreparedWorkspaceTx(tx, &prepared[index]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	if service.rebuild != nil {
		if err := service.rebuild(channelID); err != nil {
			return "", 0, err
		}
	}
	return identityUID, len(prepared), nil
}

func (service *OpenCodeGoAccountPoolService) RefreshIdentity(
	ctx context.Context,
	channelID int,
	identityUID string,
) (*model.OpenCodeGoIdentity, error) {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return nil, err
	}
	unlock := lockOpenCodeGoIdentityOperation(fmt.Sprintf("%d:identity:%s", channelID, identityUID))
	defer unlock()

	identity, err := model.GetOpenCodeGoIdentityPool(channelID, identityUID)
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, gorm.ErrRecordNotFound
	}
	scoped, err := service.scopedForIdentity(channelID, identity.UID)
	if err != nil {
		return nil, err
	}
	service = scoped
	authCookie, err := service.codec.Decrypt(
		OpenCodeGoCredentialAuthCookie,
		channelID,
		identity.UID,
		identity.AuthCookieCiphertext,
	)
	if err != nil {
		_ = service.markIdentityRefreshFailure(channelID, identity, model.OpenCodeGoIdentityStatusAuthError, err)
		return nil, err
	}
	return service.refreshIdentityWithCookie(ctx, channelID, identity, authCookie, nil, true)
}

func (service *OpenCodeGoAccountPoolService) ReplaceIdentityAuthCookie(
	ctx context.Context,
	channelID int,
	identityUID string,
	input string,
) (*model.OpenCodeGoIdentity, error) {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return nil, err
	}
	authCookie, err := NormalizeOpenCodeGoAuthCookie(input)
	if err != nil {
		return nil, err
	}
	fingerprint, err := service.codec.Fingerprint(OpenCodeGoCredentialAuthCookie, authCookie)
	if err != nil {
		return nil, err
	}
	unlockFingerprint := lockOpenCodeGoIdentityOperation(fmt.Sprintf("%d:fingerprint:%s", channelID, fingerprint))
	defer unlockFingerprint()
	unlockIdentity := lockOpenCodeGoIdentityOperation(fmt.Sprintf("%d:identity:%s", channelID, identityUID))
	defer unlockIdentity()

	identity, err := model.GetOpenCodeGoIdentityPool(channelID, identityUID)
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, gorm.ErrRecordNotFound
	}
	scoped, err := service.scopedForIdentity(channelID, identity.UID)
	if err != nil {
		return nil, err
	}
	service = scoped
	duplicate, err := model.GetOpenCodeGoIdentityByFingerprint(channelID, fingerprint)
	if err != nil {
		return nil, err
	}
	if duplicate != nil && duplicate.UID != identityUID {
		return nil, errors.New("OpenCode Go Cookie is already imported in this channel")
	}
	ciphertext, err := service.codec.Encrypt(OpenCodeGoCredentialAuthCookie, channelID, identityUID, authCookie)
	if err != nil {
		return nil, err
	}
	return service.refreshIdentityWithCookie(
		ctx,
		channelID,
		identity,
		authCookie,
		&openCodeGoIdentityCredentialUpdate{ciphertext: ciphertext, fingerprint: fingerprint},
		false,
	)
}

func (service *OpenCodeGoAccountPoolService) refreshIdentityWithCookie(
	ctx context.Context,
	channelID int,
	identity *model.OpenCodeGoIdentity,
	authCookie string,
	credentialUpdate *openCodeGoIdentityCredentialUpdate,
	markFailure bool,
) (*model.OpenCodeGoIdentity, error) {
	cachedWorkspaceID := ""
	if credentialUpdate == nil && len(identity.Workspaces) > 0 {
		cachedWorkspaceID = identity.Workspaces[0].UpstreamWorkspaceID
	}
	discovered, err := service.console.DiscoverWorkspacePages(ctx, authCookie, cachedWorkspaceID)
	if err != nil {
		status := model.OpenCodeGoIdentityStatusStale
		if errors.Is(err, ErrOpenCodeGoAuthenticationInvalid) {
			status = model.OpenCodeGoIdentityStatusAuthError
		}
		if markFailure {
			_ = service.markIdentityRefreshFailure(channelID, identity, status, err, authCookie)
		}
		return nil, err
	}

	existingByUpstreamID := make(map[string]*model.OpenCodeGoWorkspace, len(identity.Workspaces))
	for index := range identity.Workspaces {
		workspace := &identity.Workspaces[index]
		existingByUpstreamID[strings.ToLower(workspace.UpstreamWorkspaceID)] = workspace
	}
	prepared := make([]openCodeGoPreparedWorkspace, 0, len(discovered)+len(identity.Workspaces))
	seen := make(map[string]struct{})
	identityEmail := identity.Email
	for _, result := range discovered {
		key := strings.ToLower(result.Workspace.ID)
		seen[key] = struct{}{}
		workspace, prepareErr := service.prepareWorkspace(ctx, channelID, authCookie, result, existingByUpstreamID[key])
		if prepareErr != nil {
			return nil, prepareErr
		}
		if workspace.record.Email != "" {
			identityEmail = workspace.record.Email
		}
		prepared = append(prepared, workspace)
	}
	for key, existing := range existingByUpstreamID {
		if _, found := seen[key]; found {
			continue
		}
		prepared = append(prepared, prepareMissingOpenCodeGoWorkspace(*existing, service.now().Unix()))
	}

	// Discovery is intentionally outside the barrier. Once persistence begins,
	// keep relay behind the mutation until the selection epoch advances and the
	// replacement snapshot has been published.
	releaseMutation := BeginOpenCodeGoPoolMutation(channelID)
	defer releaseMutation()
	now := service.now().Unix()
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		if err := validateOpenCodeGoPoolChannelTx(tx, channelID); err != nil {
			return err
		}
		var currentIdentity model.OpenCodeGoIdentity
		if err := model.LockForUpdate(tx).
			Where("channel_id = ? AND id = ?", channelID, identity.ID).
			First(&currentIdentity).Error; err != nil {
			return err
		}
		identityStatus := model.OpenCodeGoIdentityStatusActive
		if currentIdentity.Status == model.OpenCodeGoIdentityStatusManualDisabled {
			identityStatus = model.OpenCodeGoIdentityStatusManualDisabled
		}
		identityUpdates := map[string]interface{}{}
		if currentIdentity.LastSyncedAt <= now {
			identityUpdates["email"] = identityEmail
			identityUpdates["status"] = identityStatus
			identityUpdates["last_synced_at"] = now
			identityUpdates["last_error"] = ""
			identityUpdates["updated_at"] = now
		}
		if credentialUpdate != nil {
			identityUpdates["auth_cookie_ciphertext"] = credentialUpdate.ciphertext
			identityUpdates["auth_cookie_fingerprint"] = credentialUpdate.fingerprint
		}
		if len(identityUpdates) > 0 {
			if err := tx.Model(&model.OpenCodeGoIdentity{}).Where("id = ?", currentIdentity.ID).Updates(identityUpdates).Error; err != nil {
				return err
			}
		}
		for index := range prepared {
			prepared[index].record.IdentityID = currentIdentity.ID
			if prepared[index].record.ID == 0 {
				if err := createOpenCodeGoPreparedWorkspaceTx(tx, &prepared[index]); err != nil {
					return err
				}
				continue
			}
			if err := updateOpenCodeGoPreparedWorkspaceTx(tx, &prepared[index], now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if service.rebuild != nil {
		// Workspace/API-key state may have changed, so selections from the
		// previous snapshot must become stale. Cookie refresh does not change the
		// identity's proxy binding, so advance only the selection epoch and retain
		// its clients.
		openCodeGoIdentityProxyClients.advanceSelectionGeneration(channelID)
		if err := service.rebuild(channelID); err != nil {
			return nil, err
		}
	}
	return model.GetOpenCodeGoIdentityPool(channelID, identity.UID)
}

func (service *OpenCodeGoAccountPoolService) prepareWorkspace(
	ctx context.Context,
	channelID int,
	authCookie string,
	result OpenCodeGoWorkspacePageResult,
	existing *model.OpenCodeGoWorkspace,
) (openCodeGoPreparedWorkspace, error) {
	observedAt := service.now()
	now := observedAt.Unix()
	prepared := openCodeGoPreparedWorkspace{}
	if existing != nil {
		prepared.record = *existing
	} else {
		prepared.record = model.OpenCodeGoWorkspace{
			UID:                 uuid.NewString(),
			ChannelID:           channelID,
			UpstreamWorkspaceID: result.Workspace.ID,
			ManualEnabled:       true,
			CredentialStatus:    model.OpenCodeGoCredentialPending,
			MembershipStatus:    model.OpenCodeGoMembershipUnknown,
			EffectiveState:      model.OpenCodeGoStatePending,
			QuotaSnapshotStatus: model.OpenCodeGoQuotaSnapshotPending,
		}
	}
	prepared.record.ChannelID = channelID
	prepared.record.Name = result.Workspace.Name
	prepared.record.LastSyncedAt = now
	prepared.record.LastError = ""
	prepared.record.StateReason = ""
	prepared.record.QuotaError = ""

	if result.Error != nil || result.Page == nil {
		message := sanitizeOpenCodeGoError(result.Error, authCookie)
		if message == "" {
			message = "OpenCode Go workspace page is unavailable"
		}
		prepared.record.EffectiveState = model.OpenCodeGoStateStale
		prepared.record.StateReason = message
		prepared.record.QuotaSnapshotStatus = model.OpenCodeGoQuotaSnapshotStale
		prepared.record.QuotaError = message
		prepared.record.LastError = message
		current := prepared.record
		if existing != nil {
			current = *existing
		}
		observation := OpenCodeGoHealthObservation{
			Kind:       OpenCodeGoObservationStaleSnapshot,
			ObservedAt: observedAt,
			Reason:     message,
		}
		reduced, _, reduceErr := ReduceOpenCodeGoWorkspaceHealth(
			current,
			prepared.record,
			prepared.windows,
			observation,
		)
		if reduceErr != nil {
			return prepared, reduceErr
		}
		prepared.record = reduced
		prepared.healthObservation = observation
		return prepared, nil
	}

	page := result.Page
	prepared.record.UpstreamWorkspaceID = page.WorkspaceID
	prepared.record.Name = page.WorkspaceName
	prepared.record.Email = page.Email
	prepared.record.MembershipStatus = page.MembershipStatus
	prepared.record.SubscriptionReference = page.SubscriptionReference
	prepared.record.ReferralCode = page.ReferralCode
	prepared.record.AvailableReferralRewards = page.AvailableReferralRewards
	prepared.record.UsedReferralRewards = page.UsedReferralRewards
	prepared.record.ChinaModelsEnabled = page.ChinaModelsEnabled
	prepared.record.ChinaModelsCheckedAt = now
	prepared.record.QuotaParserVersion = OpenCodeGoSSRParserVersion

	if page.Quota != nil {
		prepared.record.QuotaSnapshotStatus = model.OpenCodeGoQuotaSnapshotComplete
		prepared.record.QuotaFetchedAt = page.Quota.FetchedAt
		prepared.record.QuotaNextRefreshAt = page.Quota.NextRefreshAt
		prepared.windows = make([]model.OpenCodeGoQuotaWindow, 0, len(page.Quota.Windows))
		for _, window := range page.Quota.Windows {
			prepared.windows = append(prepared.windows, model.OpenCodeGoQuotaWindow{
				Kind:         window.Kind,
				UsedPercent:  window.UsedPercent,
				ResetSeconds: window.ResetSeconds,
				ResetAt:      window.ResetAt,
				FetchedAt:    window.FetchedAt,
			})
		}
	} else if page.QuotaParseError != "" {
		prepared.record.QuotaSnapshotStatus = model.OpenCodeGoQuotaSnapshotError
		prepared.record.QuotaError = sanitizeOpenCodeGoError(errors.New(page.QuotaParseError))
	} else {
		prepared.record.QuotaSnapshotStatus = model.OpenCodeGoQuotaSnapshotStale
	}

	apiKey, keyUsable := service.existingOpenCodeGoAPIKey(channelID, existing)
	fetchedAPIKey, keyErr := service.console.FetchAPIKey(ctx, authCookie, page.WorkspaceID)
	if keyErr != nil {
		prepared.record.LastError = sanitizeOpenCodeGoError(keyErr, authCookie, apiKey)
	} else if fetchedAPIKey != "" {
		apiKey = fetchedAPIKey
		keyUsable = true
		ciphertext, err := service.codec.Encrypt(OpenCodeGoCredentialAPIKey, channelID, prepared.record.UID, apiKey)
		if err != nil {
			return prepared, err
		}
		fingerprint, err := service.codec.Fingerprint(OpenCodeGoCredentialAPIKey, apiKey)
		if err != nil {
			return prepared, err
		}
		prepared.record.APIKeyCiphertext = ciphertext
		prepared.record.APIKeyFingerprint = fingerprint
		prepared.record.APIKeyPrefix = openCodeGoAPIKeyPrefix(apiKey)
	}
	if keyUsable {
		prepared.record.CredentialStatus = model.OpenCodeGoCredentialValid
		models, modelErr := service.console.FetchModels(ctx, apiKey)
		if modelErr == nil {
			prepared.models = models
			prepared.modelsFresh = true
		} else if prepared.record.LastError == "" {
			prepared.record.LastError = sanitizeOpenCodeGoError(modelErr, authCookie, apiKey)
		}
	} else {
		prepared.record.CredentialStatus = model.OpenCodeGoCredentialMissing
	}

	hasUsableModels := hasUsableOpenCodeGoModels(prepared.models, prepared.modelsFresh, existing)
	current := prepared.record
	if existing != nil {
		current = *existing
	}
	observation := OpenCodeGoHealthObservation{
		Kind:            OpenCodeGoObservationConsoleSnapshot,
		ObservedAt:      observedAt,
		HasUsableModels: hasUsableModels,
	}
	reduced, _, reduceErr := ReduceOpenCodeGoWorkspaceHealth(
		current,
		prepared.record,
		prepared.windows,
		observation,
	)
	if reduceErr != nil {
		return prepared, reduceErr
	}
	prepared.record = reduced
	prepared.healthObservation = observation
	if prepared.record.LastError == "" && prepared.record.StateReason != "" && prepared.record.EffectiveState == model.OpenCodeGoStateStale {
		prepared.record.LastError = prepared.record.StateReason
	}
	return prepared, nil
}

func (service *OpenCodeGoAccountPoolService) existingOpenCodeGoAPIKey(
	channelID int,
	existing *model.OpenCodeGoWorkspace,
) (string, bool) {
	if existing == nil || existing.APIKeyCiphertext == "" || existing.CredentialStatus != model.OpenCodeGoCredentialValid {
		return "", false
	}
	apiKey, err := service.codec.Decrypt(
		OpenCodeGoCredentialAPIKey,
		channelID,
		existing.UID,
		existing.APIKeyCiphertext,
	)
	return apiKey, err == nil && apiKey != ""
}

func hasUsableOpenCodeGoModels(models []string, modelsFresh bool, existing *model.OpenCodeGoWorkspace) bool {
	if modelsFresh {
		return len(models) > 0
	}
	if existing == nil {
		return false
	}
	for _, entry := range existing.Models {
		if entry.Discovered && entry.State == model.OpenCodeGoModelAvailable && entry.DisabledUntil == 0 {
			return true
		}
	}
	return false
}

func createOpenCodeGoPreparedWorkspaceTx(tx *gorm.DB, prepared *openCodeGoPreparedWorkspace) error {
	if err := tx.Create(&prepared.record).Error; err != nil {
		return err
	}
	for index := range prepared.windows {
		prepared.windows[index].WorkspaceID = prepared.record.ID
	}
	if len(prepared.windows) > 0 {
		if err := tx.Create(&prepared.windows).Error; err != nil {
			return err
		}
	}
	for _, modelID := range prepared.models {
		entry := model.OpenCodeGoWorkspaceModel{
			WorkspaceID: prepared.record.ID,
			Model:       modelID,
			Discovered:  true,
			State:       model.OpenCodeGoModelAvailable,
		}
		if err := tx.Create(&entry).Error; err != nil {
			return err
		}
	}
	return nil
}

func updateOpenCodeGoPreparedWorkspaceTx(tx *gorm.DB, prepared *openCodeGoPreparedWorkspace, now int64) error {
	workspace := prepared.record
	var current model.OpenCodeGoWorkspace
	if err := model.LockForUpdate(tx).
		Where("id = ?", workspace.ID).
		Preload("QuotaWindows").
		Preload("Models").
		First(&current).Error; err != nil {
		return err
	}
	if workspace.LastSyncedAt < current.LastSyncedAt {
		prepared.record = current
		prepared.windows = nil
		prepared.modelsFresh = false
		return nil
	}
	workspace.ManualEnabled = current.ManualEnabled
	copyOpenCodeGoWorkspaceHealthOutputs(&workspace, current)
	reduced, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		current,
		workspace,
		prepared.windows,
		prepared.healthObservation,
	)
	if err != nil {
		return err
	}
	if applied {
		workspace = reduced
		if workspace.EffectiveState != model.OpenCodeGoStateEligible && workspace.LastError == "" {
			workspace.LastError = workspace.StateReason
		}
	} else {
		workspace.CredentialStatus = current.CredentialStatus
		workspace.QuotaNextRefreshAt = current.QuotaNextRefreshAt
		workspace.LastError = current.LastError
	}
	if workspace.LastSyncedAt < current.LastSyncedAt {
		workspace.LastSyncedAt = current.LastSyncedAt
	}
	if len(prepared.windows) > 0 && workspace.QuotaFetchedAt < current.QuotaFetchedAt {
		workspace.QuotaSnapshotStatus = current.QuotaSnapshotStatus
		workspace.QuotaFetchedAt = current.QuotaFetchedAt
		workspace.QuotaNextRefreshAt = current.QuotaNextRefreshAt
		workspace.QuotaParserVersion = current.QuotaParserVersion
		workspace.QuotaError = current.QuotaError
		prepared.windows = nil
	}
	if prepared.modelsFresh {
		for _, entry := range current.Models {
			if entry.UpdatedAt > prepared.healthObservation.ObservedAt.Unix() {
				prepared.modelsFresh = false
				break
			}
		}
	}
	prepared.record = workspace
	updates := map[string]interface{}{
		"upstream_workspace_id":      workspace.UpstreamWorkspaceID,
		"name":                       workspace.Name,
		"email":                      workspace.Email,
		"api_key_ciphertext":         workspace.APIKeyCiphertext,
		"api_key_fingerprint":        workspace.APIKeyFingerprint,
		"api_key_prefix":             workspace.APIKeyPrefix,
		"credential_status":          workspace.CredentialStatus,
		"membership_status":          workspace.MembershipStatus,
		"subscription_reference":     workspace.SubscriptionReference,
		"subscription_ends_at":       workspace.SubscriptionEndsAt,
		"renewal_cancelled_at":       workspace.RenewalCancelledAt,
		"renewal_checked_at":         workspace.RenewalCheckedAt,
		"renewal_cancel_error":       workspace.RenewalCancelError,
		"manual_enabled":             workspace.ManualEnabled,
		"effective_state":            workspace.EffectiveState,
		"state_reason":               workspace.StateReason,
		"health_observation":         workspace.HealthObservation,
		"health_observed_at":         workspace.HealthObservedAt,
		"cooldown_until":             workspace.CooldownUntil,
		"quota_snapshot_status":      workspace.QuotaSnapshotStatus,
		"quota_fetched_at":           workspace.QuotaFetchedAt,
		"quota_next_refresh_at":      workspace.QuotaNextRefreshAt,
		"quota_recovery_at":          workspace.QuotaRecoveryAt,
		"quota_parser_version":       workspace.QuotaParserVersion,
		"quota_error":                workspace.QuotaError,
		"china_models_enabled":       workspace.ChinaModelsEnabled,
		"china_models_checked_at":    workspace.ChinaModelsCheckedAt,
		"china_models_error":         workspace.ChinaModelsError,
		"referral_code":              workspace.ReferralCode,
		"available_referral_rewards": workspace.AvailableReferralRewards,
		"used_referral_rewards":      workspace.UsedReferralRewards,
		"referral_reward_applied_at": workspace.ReferralRewardAppliedAt,
		"risk_detected_at":           workspace.RiskDetectedAt,
		"risk_last_checked_at":       workspace.RiskLastCheckedAt,
		"last_synced_at":             workspace.LastSyncedAt,
		"last_error":                 workspace.LastError,
		"updated_at":                 now,
	}
	if err := tx.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", workspace.ID).Updates(updates).Error; err != nil {
		return err
	}
	if len(prepared.windows) > 0 {
		if err := tx.Where("workspace_id = ?", workspace.ID).Delete(&model.OpenCodeGoQuotaWindow{}).Error; err != nil {
			return err
		}
		for index := range prepared.windows {
			prepared.windows[index].ID = 0
			prepared.windows[index].WorkspaceID = workspace.ID
		}
		if err := tx.Create(&prepared.windows).Error; err != nil {
			return err
		}
	}
	if prepared.modelsFresh {
		if err := tx.Model(&model.OpenCodeGoWorkspaceModel{}).
			Where("workspace_id = ?", workspace.ID).
			Updates(map[string]interface{}{"discovered": false, "updated_at": now}).Error; err != nil {
			return err
		}
		for _, modelID := range prepared.models {
			entry := model.OpenCodeGoWorkspaceModel{
				WorkspaceID: workspace.ID,
				Model:       modelID,
				Discovered:  true,
				State:       model.OpenCodeGoModelAvailable,
				UpdatedAt:   now,
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "workspace_id"}, {Name: "model"}},
				DoUpdates: clause.Assignments(map[string]interface{}{"discovered": true, "updated_at": now}),
			}).Create(&entry).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func copyOpenCodeGoWorkspaceHealthOutputs(target *model.OpenCodeGoWorkspace, source model.OpenCodeGoWorkspace) {
	if target == nil {
		return
	}
	target.EffectiveState = source.EffectiveState
	target.StateReason = source.StateReason
	target.HealthObservation = source.HealthObservation
	target.HealthObservedAt = source.HealthObservedAt
	target.CooldownUntil = source.CooldownUntil
	target.QuotaRecoveryAt = source.QuotaRecoveryAt
	target.RiskDetectedAt = source.RiskDetectedAt
	target.RiskLastCheckedAt = source.RiskLastCheckedAt
}

func prepareMissingOpenCodeGoWorkspace(existing model.OpenCodeGoWorkspace, now int64) openCodeGoPreparedWorkspace {
	reason := "workspace was not returned by the latest console discovery"
	existing.QuotaSnapshotStatus = model.OpenCodeGoQuotaSnapshotStale
	existing.QuotaError = reason
	existing.LastError = reason
	existing.LastSyncedAt = now
	observation := OpenCodeGoHealthObservation{
		Kind:       OpenCodeGoObservationStaleSnapshot,
		ObservedAt: time.Unix(now, 0),
		Reason:     reason,
	}
	reduced, _, err := ReduceOpenCodeGoWorkspaceHealth(
		existing,
		existing,
		existing.QuotaWindows,
		observation,
	)
	if err == nil {
		existing = reduced
	}
	return openCodeGoPreparedWorkspace{record: existing, healthObservation: observation}
}

func (service *OpenCodeGoAccountPoolService) markIdentityRefreshFailure(
	channelID int,
	identity *model.OpenCodeGoIdentity,
	status string,
	err error,
	secrets ...string,
) error {
	message := sanitizeOpenCodeGoError(err, secrets...)
	observedAt := service.now()
	now := observedAt.Unix()
	observationKind := OpenCodeGoObservationStaleSnapshot
	if status == model.OpenCodeGoIdentityStatusAuthError {
		observationKind = OpenCodeGoObservationAuthenticationFailure
	}
	// Transient refresh failures preserve the last complete relay snapshot.
	// Authentication failures revoke the identity and therefore need the full
	// commit-to-invalidation barrier.
	releaseMutation := func() {}
	if status == model.OpenCodeGoIdentityStatusAuthError {
		releaseMutation = BeginOpenCodeGoPoolMutation(channelID)
	}
	defer releaseMutation()
	dbErr := model.DB.Transaction(func(tx *gorm.DB) error {
		var currentIdentity model.OpenCodeGoIdentity
		if err := model.LockForUpdate(tx).
			Where("channel_id = ? AND id = ?", channelID, identity.ID).
			First(&currentIdentity).Error; err != nil {
			return err
		}
		identityStatus := currentIdentity.Status
		identityUpdateAllowed := currentIdentity.LastSyncedAt < now ||
			(currentIdentity.LastSyncedAt == now &&
				openCodeGoIdentityStatusPriority(currentIdentity.Status) < openCodeGoIdentityStatusPriority(status))
		if currentIdentity.Status == model.OpenCodeGoIdentityStatusAuthError && status == model.OpenCodeGoIdentityStatusStale {
			identityUpdateAllowed = false
		}
		if currentIdentity.Status == model.OpenCodeGoIdentityStatusManualDisabled {
			identityUpdateAllowed = false
		}
		if identityUpdateAllowed {
			// A transient console failure is not evidence that an active identity
			// or its last complete quota snapshot became invalid. Keep routing from
			// that snapshot while recording the failed refresh attempt. Explicit
			// authentication failures still invalidate the identity immediately.
			if status != model.OpenCodeGoIdentityStatusStale || currentIdentity.Status != model.OpenCodeGoIdentityStatusActive {
				identityStatus = status
			}
			if err := tx.Model(&model.OpenCodeGoIdentity{}).Where("id = ?", currentIdentity.ID).Updates(map[string]interface{}{
				"status":         identityStatus,
				"last_synced_at": now,
				"last_error":     message,
				"updated_at":     now,
			}).Error; err != nil {
				return err
			}
		}

		var workspaces []model.OpenCodeGoWorkspace
		if err := model.LockForUpdate(tx).
			Where("identity_id = ?", currentIdentity.ID).
			Order("id asc").
			Preload("QuotaWindows").
			Find(&workspaces).Error; err != nil {
			return err
		}
		for index := range workspaces {
			workspace := workspaces[index]
			if status == model.OpenCodeGoIdentityStatusStale &&
				workspace.QuotaSnapshotStatus == model.OpenCodeGoQuotaSnapshotComplete &&
				workspace.QuotaFetchedAt > 0 {
				lastSyncedAt := workspace.LastSyncedAt
				if lastSyncedAt < now {
					lastSyncedAt = now
				}
				if err := tx.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", workspace.ID).Updates(map[string]interface{}{
					"quota_error":    message,
					"last_error":     message,
					"last_synced_at": lastSyncedAt,
					"updated_at":     now,
				}).Error; err != nil {
					return err
				}
				continue
			}
			observation := OpenCodeGoHealthObservation{
				Kind:       observationKind,
				ObservedAt: observedAt,
				Reason:     message,
			}
			if !canApplyOpenCodeGoWorkspaceObservation(workspace, observation) {
				continue
			}
			candidate := workspace
			candidate.QuotaSnapshotStatus = model.OpenCodeGoQuotaSnapshotStale
			candidate.QuotaError = message
			candidate.LastError = message
			if candidate.LastSyncedAt < now {
				candidate.LastSyncedAt = now
			}
			reduced, applied, reduceErr := ReduceOpenCodeGoWorkspaceHealth(
				workspace,
				candidate,
				workspace.QuotaWindows,
				observation,
			)
			if reduceErr != nil {
				return reduceErr
			}
			updates := map[string]interface{}{
				"quota_snapshot_status": candidate.QuotaSnapshotStatus,
				"quota_error":           candidate.QuotaError,
				"last_error":            candidate.LastError,
				"last_synced_at":        candidate.LastSyncedAt,
				"updated_at":            now,
			}
			if applied {
				updates["effective_state"] = reduced.EffectiveState
				updates["state_reason"] = reduced.StateReason
				updates["health_observation"] = reduced.HealthObservation
				updates["health_observed_at"] = reduced.HealthObservedAt
				updates["quota_recovery_at"] = reduced.QuotaRecoveryAt
				updates["cooldown_until"] = reduced.CooldownUntil
			}
			if err := tx.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", workspace.ID).Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if dbErr != nil {
		return dbErr
	}
	if status == model.OpenCodeGoIdentityStatusAuthError {
		InvalidateOpenCodeGoIdentityProxyChannel(channelID)
	}
	if service.rebuild == nil {
		return nil
	}
	return service.rebuild(channelID)
}

func openCodeGoIdentityStatusPriority(status string) int {
	switch status {
	case model.OpenCodeGoIdentityStatusManualDisabled:
		return 100
	case model.OpenCodeGoIdentityStatusAuthError:
		return 80
	case model.OpenCodeGoIdentityStatusStale:
		return 60
	case model.OpenCodeGoIdentityStatusActive:
		return 10
	default:
		return 0
	}
}

func validateOpenCodeGoPoolChannel(channelID int) error {
	channel, err := model.GetChannelById(channelID, true)
	if err != nil {
		return err
	}
	if channel.Type != constant.ChannelTypeOpenCodeGo {
		return errors.New("channel is not an OpenCode Go channel")
	}
	return nil
}

func validateOpenCodeGoPoolChannelTx(tx *gorm.DB, channelID int) error {
	var channel model.Channel
	if err := tx.Select("id", "type").Where("id = ?", channelID).First(&channel).Error; err != nil {
		return err
	}
	if channel.Type != constant.ChannelTypeOpenCodeGo {
		return errors.New("channel is not an OpenCode Go channel")
	}
	return nil
}

func lockOpenCodeGoIdentityOperation(key string) func() {
	value, _ := openCodeGoIdentityOperationLocks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func sanitizeOpenCodeGoError(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	message = openCodeGoSecretPattern.ReplaceAllString(message, "[redacted-key]")
	message = openCodeGoCookiePattern.ReplaceAllString(message, "auth=[redacted]")
	message = openCodeGoUpstreamIDPattern.ReplaceAllString(message, "[workspace]")
	message = openCodeGoReferralIDPattern.ReplaceAllString(message, "[referral]")
	message = openCodeGoPortalSessionPattern.ReplaceAllString(message, "[portal-session]")
	message = openCodeGoPortalPathPattern.ReplaceAllString(message, "$1[portal-session]")
	message = openCodeGoIdentityUIDPattern.ReplaceAllString(message, "[identity]")
	message = openCodeGoProxyUserinfoPattern.ReplaceAllString(message, "[redacted-proxy]")
	message = openCodeGoAuthorizationPattern.ReplaceAllString(message, "$1[redacted]")
	message = strings.ToValidUTF8(message, "\uFFFD")
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 512 {
		message = message[:512]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

func openCodeGoAPIKeyPrefix(apiKey string) string {
	const visible = 7
	if len(apiKey) <= visible {
		return "configured"
	}
	return apiKey[:visible] + "..."
}

func firstNonEmptyOpenCodeGoMessage(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
