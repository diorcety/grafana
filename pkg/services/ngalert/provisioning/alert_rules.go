package provisioning

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/auth/identity"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/ngalert/accesscontrol"
	"github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/notifier"
	"github.com/grafana/grafana/pkg/services/ngalert/store"
	"github.com/grafana/grafana/pkg/services/quota"
	"github.com/grafana/grafana/pkg/services/user"
	"github.com/grafana/grafana/pkg/util"
)

type ruleAccessControlService interface {
	AuthorizeAccessToRuleGroup(ctx context.Context, user identity.Requester, rules models.RulesGroup) error
	AuthorizeRuleChanges(ctx context.Context, user identity.Requester, change *store.GroupDelta) error
	// CanReadAllRules returns true if the user has full access to read rules via provisioning API and bypass regular checks
	CanReadAllRules(ctx context.Context, user identity.Requester) (bool, error)
	// CanWriteAllRules returns true if the user has full access to write rules via provisioning API and bypass regular checks
	CanWriteAllRules(ctx context.Context, user identity.Requester) (bool, error)
}

type NotificationSettingsValidatorProvider interface {
	Validator(ctx context.Context, orgID int64) (notifier.NotificationSettingsValidator, error)
}

type AlertRuleService struct {
	defaultIntervalSeconds int64
	baseIntervalSeconds    int64
	rulesPerRuleGroupLimit int64
	ruleStore              RuleStore
	provenanceStore        ProvisioningStore
	dashboardService       dashboards.DashboardService
	quotas                 QuotaChecker
	xact                   TransactionManager
	log                    log.Logger
	nsValidatorProvider    NotificationSettingsValidatorProvider
	authz                  ruleAccessControlService
}

// NewAlertRuleServiceWithBypassPermissions creates a AlertRuleService that does not validate user access to perform read\write operations on rules.
func NewAlertRuleServiceWithBypassPermissions(ruleStore RuleStore,
	provenanceStore ProvisioningStore,
	dashboardService dashboards.DashboardService,
	quotas QuotaChecker,
	xact TransactionManager,
	defaultIntervalSeconds int64,
	baseIntervalSeconds int64,
	log log.Logger) *AlertRuleService {
	return &AlertRuleService{
		defaultIntervalSeconds: defaultIntervalSeconds,
		baseIntervalSeconds:    baseIntervalSeconds,
		ruleStore:              ruleStore,
		provenanceStore:        provenanceStore,
		dashboardService:       dashboardService,
		quotas:                 quotas,
		xact:                   xact,
		log:                    log,
		authz:                  &allAccessControlService{},
	}
}

func NewAlertRuleService(ruleStore RuleStore,
	provenanceStore ProvisioningStore,
	dashboardService dashboards.DashboardService,
	quotas QuotaChecker,
	xact TransactionManager,
	defaultIntervalSeconds int64,
	baseIntervalSeconds int64,
	rulesPerRuleGroupLimit int64,
	log log.Logger,
	ns NotificationSettingsValidatorProvider,
	authz *accesscontrol.RuleService,
) *AlertRuleService {
	return &AlertRuleService{
		defaultIntervalSeconds: defaultIntervalSeconds,
		baseIntervalSeconds:    baseIntervalSeconds,
		rulesPerRuleGroupLimit: rulesPerRuleGroupLimit,
		ruleStore:              ruleStore,
		provenanceStore:        provenanceStore,
		dashboardService:       dashboardService,
		quotas:                 quotas,
		xact:                   xact,
		log:                    log,
		nsValidatorProvider:    ns,
		authz:                  newRuleAccessControlService(authz),
	}
}

func (service *AlertRuleService) GetAlertRules(ctx context.Context, user identity.Requester, orgID int64) ([]*models.AlertRule, map[string]models.Provenance, error) {
	q := models.ListAlertRulesQuery{
		OrgID: orgID,
	}
	rules, err := service.ruleStore.ListAlertRules(ctx, &q)
	if err != nil {
		return nil, nil, err
	}
	provenances := make(map[string]models.Provenance)
	if len(rules) > 0 {
		resourceType := rules[0].ResourceType()
		provenances, err = service.provenanceStore.GetProvenances(ctx, orgID, resourceType)
		if err != nil {
			return nil, nil, err
		}
	}

	if can, err := service.authz.CanReadAllRules(ctx, user); !can || err != nil {
		if err != nil {
			return nil, nil, err
		}
		groups := models.GroupByAlertRuleGroupKey(rules)
		result := make([]*models.AlertRule, 0, len(rules))
		for _, group := range groups {
			if err := service.authz.AuthorizeAccessToRuleGroup(ctx, user, group); err != nil {
				if accesscontrol.IsAuthorizationError(err) {
					// remove provenances for rules that will not be added to the output
					for _, rule := range group {
						delete(provenances, rule.ResourceID())
					}
					continue
				}
				return nil, nil, err
			}
			result = append(result, group...)
		}
		rules = result
	}
	return rules, provenances, nil
}

func (service *AlertRuleService) getAlertRuleAuthorized(ctx context.Context, user identity.Requester, orgID int64, ruleUID string) (models.AlertRule, error) {
	// check if the user can read all rules. If it cannot, pull the entire group and verify access to the entire group.
	if can, err := service.authz.CanReadAllRules(ctx, user); !can || err != nil {
		if err != nil {
			return models.AlertRule{}, err
		}
		// if user is not Grafana Admin check that the user can read this rule.
		// to check that we need to fetch the group to which the rule belongs to.
		q := &models.GetAlertRulesGroupByRuleUIDQuery{
			UID:   ruleUID,
			OrgID: orgID,
		}
		group, err := service.ruleStore.GetAlertRulesGroupByRuleUID(ctx, q)
		if err != nil {
			return models.AlertRule{}, err
		}
		if len(group) == 0 {
			return models.AlertRule{}, models.ErrAlertRuleNotFound
		}
		if err := service.authz.AuthorizeAccessToRuleGroup(ctx, user, group); err != nil {
			return models.AlertRule{}, err
		}
		for _, rule := range group {
			if rule.UID == ruleUID {
				return *rule, nil
			}
		}
		return models.AlertRule{}, models.ErrAlertRuleNotFound
	}
	// otherwise, just pull the specific rule by UID
	query := &models.GetAlertRuleByUIDQuery{
		OrgID: orgID,
		UID:   ruleUID,
	}
	rule, err := service.ruleStore.GetAlertRuleByUID(ctx, query)
	if err != nil {
		return models.AlertRule{}, err
	}
	if rule == nil {
		return models.AlertRule{}, models.ErrAlertRuleNotFound
	}
	return *rule, nil
}

func (service *AlertRuleService) GetAlertRule(ctx context.Context, user identity.Requester, orgID int64, ruleUID string) (models.AlertRule, models.Provenance, error) {
	rule, err := service.getAlertRuleAuthorized(ctx, user, orgID, ruleUID)
	if err != nil {
		return models.AlertRule{}, models.ProvenanceNone, err
	}
	provenance, err := service.provenanceStore.GetProvenance(ctx, &rule, orgID)
	if err != nil {
		return models.AlertRule{}, models.ProvenanceNone, err
	}
	return rule, provenance, nil
}

type AlertRuleWithFolderTitle struct {
	AlertRule   models.AlertRule
	FolderTitle string
}

// GetAlertRuleWithFolderTitle returns a single alert rule with its folder title.
func (service *AlertRuleService) GetAlertRuleWithFolderTitle(ctx context.Context, user identity.Requester, orgID int64, ruleUID string) (AlertRuleWithFolderTitle, error) {
	rule, err := service.getAlertRuleAuthorized(ctx, user, orgID, ruleUID)
	if err != nil {
		return AlertRuleWithFolderTitle{}, err
	}

	dq := dashboards.GetDashboardQuery{
		OrgID: orgID,
		UID:   rule.NamespaceUID,
	}

	dash, err := service.dashboardService.GetDashboard(ctx, &dq)
	if err != nil {
		return AlertRuleWithFolderTitle{}, err
	}

	return AlertRuleWithFolderTitle{
		AlertRule:   rule,
		FolderTitle: dash.Title,
	}, nil
}

// CreateAlertRule creates a new alert rule. This function will ignore any
// interval that is set in the rule struct and use the already existing group
// interval or the default one.
func (service *AlertRuleService) CreateAlertRule(ctx context.Context, user *user.SignedInUser, rule models.AlertRule, provenance models.Provenance) (models.AlertRule, error) {
	if rule.UID == "" {
		rule.UID = util.GenerateShortUID()
	} else if err := util.ValidateUID(rule.UID); err != nil {
		return models.AlertRule{}, errors.Join(models.ErrAlertRuleFailedValidation, fmt.Errorf("cannot create rule with UID '%s': %w", rule.UID, err))
	}
	var interval = service.defaultIntervalSeconds
	// check if user can bypass fine-grained rule authorization checks. If it cannot, verfiy that the user can add rules to the group
	if can, err := service.authz.CanWriteAllRules(ctx, user); !can || err != nil {
		if err != nil {
			return models.AlertRule{}, err
		}
		delta, err := store.CalculateRuleCreate(ctx, service.ruleStore, &rule)
		if err != nil {
			return models.AlertRule{}, fmt.Errorf("failed to calculate delta: %w", err)
		}
		if err := service.authz.AuthorizeRuleChanges(ctx, user, delta); err != nil {
			return models.AlertRule{}, err
		}
		existingGroup := delta.AffectedGroups[rule.GetGroupKey()]
		if len(existingGroup) > 0 {
			interval = existingGroup[0].IntervalSeconds
		}
	} else {
		groupInterval, err := service.ruleStore.GetRuleGroupInterval(ctx, rule.OrgID, rule.NamespaceUID, rule.RuleGroup)
		// if the alert group does not exists we just use the default interval
		if err != nil {
			if !errors.Is(err, store.ErrAlertRuleGroupNotFound) {
				return models.AlertRule{}, err
			}
		} else {
			interval = groupInterval
		}
	}

	rule.IntervalSeconds = interval
	err := rule.SetDashboardAndPanelFromAnnotations()
	if err != nil {
		return models.AlertRule{}, err
	}
	rule.Updated = time.Now()
	if len(rule.NotificationSettings) > 0 {
		validator, err := service.nsValidatorProvider.Validator(ctx, rule.OrgID)
		if err != nil {
			return models.AlertRule{}, err
		}
		for _, setting := range rule.NotificationSettings {
			if err := validator.Validate(setting); err != nil {
				return models.AlertRule{}, err
			}
		}
	}
	err = service.xact.InTransaction(ctx, func(ctx context.Context) error {
		ids, err := service.ruleStore.InsertAlertRules(ctx, []models.AlertRule{
			rule,
		})
		if err != nil {
			return err
		}
		var fixed bool
		for _, key := range ids {
			if key.UID == rule.UID {
				rule.ID = key.ID
				fixed = true
				break
			}
		}
		if !fixed {
			return errors.New("couldn't find newly created id")
		}

		if err = service.checkLimitsTransactionCtx(ctx, rule.OrgID, user.UserID); err != nil {
			return err
		}

		return service.provenanceStore.SetProvenance(ctx, &rule, rule.OrgID, provenance)
	})
	if err != nil {
		return models.AlertRule{}, err
	}
	return rule, nil
}

func (service *AlertRuleService) GetRuleGroup(ctx context.Context, user identity.Requester, orgID int64, namespaceUID, group string) (models.AlertRuleGroup, error) {
	q := models.ListAlertRulesQuery{
		OrgID:         orgID,
		NamespaceUIDs: []string{namespaceUID},
		RuleGroup:     group,
	}
	ruleList, err := service.ruleStore.ListAlertRules(ctx, &q)
	if err != nil {
		return models.AlertRuleGroup{}, err
	}
	if len(ruleList) == 0 {
		return models.AlertRuleGroup{}, store.ErrAlertRuleGroupNotFound
	}
	if can, err := service.authz.CanReadAllRules(ctx, user); !can || err != nil {
		if err != nil {
			return models.AlertRuleGroup{}, err
		}
		if err := service.authz.AuthorizeAccessToRuleGroup(ctx, user, ruleList); err != nil {
			return models.AlertRuleGroup{}, err
		}
	}
	res := models.AlertRuleGroup{
		Title:     ruleList[0].RuleGroup,
		FolderUID: ruleList[0].NamespaceUID,
		Interval:  ruleList[0].IntervalSeconds,
		Rules:     make([]models.AlertRule, 0, len(ruleList)),
	}
	for _, r := range ruleList {
		if r != nil {
			res.Rules = append(res.Rules, *r)
		}
	}
	return res, nil
}

// UpdateRuleGroup will update the interval for all rules in the group.
func (service *AlertRuleService) UpdateRuleGroup(ctx context.Context, user identity.Requester, orgID int64, namespaceUID string, ruleGroup string, intervalSeconds int64) error {
	if err := models.ValidateRuleGroupInterval(intervalSeconds, service.baseIntervalSeconds); err != nil {
		return err
	}
	return service.xact.InTransaction(ctx, func(ctx context.Context) error {
		query := &models.ListAlertRulesQuery{
			OrgID:         orgID,
			NamespaceUIDs: []string{namespaceUID},
			RuleGroup:     ruleGroup,
		}
		ruleList, err := service.ruleStore.ListAlertRules(ctx, query)
		if err != nil {
			return fmt.Errorf("failed to list alert rules: %w", err)
		}
		updateRules := make([]models.UpdateRule, 0, len(ruleList))
		for _, rule := range ruleList {
			if rule.IntervalSeconds == intervalSeconds {
				continue
			}
			newRule := *rule
			newRule.IntervalSeconds = intervalSeconds
			updateRules = append(updateRules, models.UpdateRule{
				Existing: rule,
				New:      newRule,
			})
		}

		// check if user has write access to all rules and can bypass the regular checks.
		// If it cannot, check that the user is authorized to perform all the changes caused by this request
		if can, err := service.authz.CanWriteAllRules(ctx, user); !can || err != nil {
			if err != nil {
				return err
			}
			groupKey := models.AlertRuleGroupKey{
				OrgID:        user.GetOrgID(),
				NamespaceUID: namespaceUID,
				RuleGroup:    ruleGroup,
			}
			ruleDeltas := make([]store.RuleDelta, 0, len(ruleList))
			for _, upd := range updateRules {
				updNew := upd.New
				ruleDeltas = append(ruleDeltas, store.RuleDelta{
					Existing: upd.Existing,
					New:      &updNew,
				})
			}
			delta := &store.GroupDelta{
				GroupKey: groupKey,
				AffectedGroups: map[models.AlertRuleGroupKey]models.RulesGroup{
					groupKey: ruleList,
				},
				Update: ruleDeltas,
			}
			if err := service.authz.AuthorizeRuleChanges(ctx, user, delta); err != nil {
				return err
			}
		}

		return service.ruleStore.UpdateAlertRules(ctx, updateRules)
	})
}

func (service *AlertRuleService) ReplaceRuleGroup(ctx context.Context, user *user.SignedInUser, orgID int64, group models.AlertRuleGroup, provenance models.Provenance) error {
	if err := models.ValidateRuleGroupInterval(group.Interval, service.baseIntervalSeconds); err != nil {
		return err
	}

	delta, err := service.calcDelta(ctx, orgID, group)
	if err != nil {
		return err
	}

	if delta.IsEmpty() {
		return nil
	}

	// check if the current user has permissions to all rules and can bypass the regular authorization validation.
	if can, err := service.authz.CanWriteAllRules(ctx, user); !can || err != nil {
		if err != nil {
			return err
		}
		if err := service.authz.AuthorizeRuleChanges(ctx, user, delta); err != nil {
			return err
		}

	}

	newOrUpdatedNotificationSettings := delta.NewOrUpdatedNotificationSettings()
	if len(newOrUpdatedNotificationSettings) > 0 {
		validator, err := service.nsValidatorProvider.Validator(ctx, delta.GroupKey.OrgID)
		if err != nil {
			return err
		}
		for _, s := range newOrUpdatedNotificationSettings {
			if err := validator.Validate(s); err != nil {
				return errors.Join(models.ErrAlertRuleFailedValidation, err)
			}
		}
	}

	return service.persistDelta(ctx, orgID, delta, user, provenance)
}

func (service *AlertRuleService) DeleteRuleGroup(ctx context.Context, user *user.SignedInUser, orgID int64, namespaceUID, group string, provenance models.Provenance) error {
	delta, err := store.CalculateRuleGroupDelete(ctx, service.ruleStore, models.AlertRuleGroupKey{
		OrgID:        orgID,
		NamespaceUID: namespaceUID,
		RuleGroup:    group,
	})
	if err != nil {
		return err
	}

	// check if the current user has permissions to all rules and can bypass the regular authorization validation.
	if can, err := service.authz.CanWriteAllRules(ctx, user); !can || err != nil {
		if err != nil {
			return err
		}
		if err := service.authz.AuthorizeRuleChanges(ctx, user, delta); err != nil {
			return err
		}
	}

	return service.persistDelta(ctx, orgID, delta, user, provenance)
}

func (service *AlertRuleService) calcDelta(ctx context.Context, orgID int64, group models.AlertRuleGroup) (*store.GroupDelta, error) {
	// If the provided request did not provide the rules list at all, treat it as though it does not wish to change rules.
	// This is done for backwards compatibility. Requests which specify only the interval must update only the interval.
	if group.Rules == nil {
		listRulesQuery := models.ListAlertRulesQuery{
			OrgID:         orgID,
			NamespaceUIDs: []string{group.FolderUID},
			RuleGroup:     group.Title,
		}
		ruleList, err := service.ruleStore.ListAlertRules(ctx, &listRulesQuery)
		if err != nil {
			return nil, fmt.Errorf("failed to list alert rules: %w", err)
		}
		group.Rules = make([]models.AlertRule, 0, len(ruleList))
		for _, r := range ruleList {
			if r != nil {
				group.Rules = append(group.Rules, *r)
			}
		}
	}

	if err := service.checkGroupLimits(group); err != nil {
		return nil, fmt.Errorf("write rejected due to exceeded limits: %w", err)
	}

	key := models.AlertRuleGroupKey{
		OrgID:        orgID,
		NamespaceUID: group.FolderUID,
		RuleGroup:    group.Title,
	}
	rules := make([]*models.AlertRuleWithOptionals, 0, len(group.Rules))
	group = *syncGroupRuleFields(&group, orgID)
	for i := range group.Rules {
		if err := group.Rules[i].SetDashboardAndPanelFromAnnotations(); err != nil {
			return nil, err
		}
		rules = append(rules, &models.AlertRuleWithOptionals{AlertRule: group.Rules[i], HasPause: true})
	}
	delta, err := store.CalculateChanges(ctx, service.ruleStore, key, rules)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate diff for alert rules: %w", err)
	}

	// Refresh all calculated fields across all rules.
	return store.UpdateCalculatedRuleFields(delta), nil
}

func (service *AlertRuleService) persistDelta(ctx context.Context, orgID int64, delta *store.GroupDelta, user *user.SignedInUser, provenance models.Provenance) error {
	return service.xact.InTransaction(ctx, func(ctx context.Context) error {
		// Delete first as this could prevent future unique constraint violations.
		if len(delta.Delete) > 0 {
			for _, del := range delta.Delete {
				// check that provenance is not changed in an invalid way
				storedProvenance, err := service.provenanceStore.GetProvenance(ctx, del, orgID)
				if err != nil {
					return err
				}
				if canUpdate := canUpdateProvenanceInRuleGroup(storedProvenance, provenance); !canUpdate {
					return fmt.Errorf("cannot delete with provided provenance '%s', needs '%s'", provenance, storedProvenance)
				}
			}
			if err := service.deleteRules(ctx, orgID, delta.Delete...); err != nil {
				return err
			}
		}

		if len(delta.Update) > 0 {
			updates := make([]models.UpdateRule, 0, len(delta.Update))
			for _, update := range delta.Update {
				// check that provenance is not changed in an invalid way
				storedProvenance, err := service.provenanceStore.GetProvenance(ctx, update.New, orgID)
				if err != nil {
					return err
				}
				if canUpdate := canUpdateProvenanceInRuleGroup(storedProvenance, provenance); !canUpdate {
					return fmt.Errorf("cannot update with provided provenance '%s', needs '%s'", provenance, storedProvenance)
				}
				updates = append(updates, models.UpdateRule{
					Existing: update.Existing,
					New:      *update.New,
				})
			}
			if err := service.ruleStore.UpdateAlertRules(ctx, updates); err != nil {
				return fmt.Errorf("failed to update alert rules: %w", err)
			}
			for _, update := range delta.Update {
				if err := service.provenanceStore.SetProvenance(ctx, update.New, orgID, provenance); err != nil {
					return err
				}
			}
		}

		if len(delta.New) > 0 {
			uids, err := service.ruleStore.InsertAlertRules(ctx, withoutNilAlertRules(delta.New))
			if err != nil {
				return fmt.Errorf("failed to insert alert rules: %w", err)
			}
			for _, key := range uids {
				if err := service.provenanceStore.SetProvenance(ctx, &models.AlertRule{UID: key.UID}, orgID, provenance); err != nil {
					return err
				}
			}
		}

		if err := service.checkLimitsTransactionCtx(ctx, orgID, user.UserID); err != nil {
			return err
		}

		return nil
	})
}

// UpdateAlertRule updates an alert rule.
func (service *AlertRuleService) UpdateAlertRule(ctx context.Context, user identity.Requester, rule models.AlertRule, provenance models.Provenance) (models.AlertRule, error) {
	var storedRule *models.AlertRule
	// check if the user has full access to all rules and can bypass the regular authorization validations.
	// If it cannot, calculate the changes to the group caused by this update and authorize them.
	if can, err := service.authz.CanWriteAllRules(ctx, user); !can || err != nil {
		if err != nil {
			return models.AlertRule{}, err
		}
		delta, err := store.CalculateRuleUpdate(ctx, service.ruleStore, &models.AlertRuleWithOptionals{AlertRule: rule})
		if err != nil {
			return models.AlertRule{}, err
		}
		if err = service.authz.AuthorizeRuleChanges(ctx, user, delta); err != nil {
			return models.AlertRule{}, err
		}
	} else {
		query := &models.GetAlertRuleByUIDQuery{
			OrgID: rule.OrgID,
			UID:   rule.UID,
		}
		existing, err := service.ruleStore.GetAlertRuleByUID(ctx, query)
		if err != nil {
			return models.AlertRule{}, err
		}
		storedRule = existing
	}
	storedProvenance, err := service.provenanceStore.GetProvenance(ctx, storedRule, storedRule.OrgID)
	if err != nil {
		return models.AlertRule{}, err
	}
	if storedProvenance != provenance && storedProvenance != models.ProvenanceNone {
		return models.AlertRule{}, fmt.Errorf("cannot change provenance from '%s' to '%s'", storedProvenance, provenance)
	}
	if len(rule.NotificationSettings) > 0 {
		validator, err := service.nsValidatorProvider.Validator(ctx, rule.OrgID)
		if err != nil {
			return models.AlertRule{}, err
		}
		for _, setting := range rule.NotificationSettings {
			if err := validator.Validate(setting); err != nil {
				return models.AlertRule{}, err
			}
		}
	}
	rule.Updated = time.Now()
	rule.ID = storedRule.ID
	rule.IntervalSeconds = storedRule.IntervalSeconds
	err = rule.SetDashboardAndPanelFromAnnotations()
	if err != nil {
		return models.AlertRule{}, err
	}
	err = service.xact.InTransaction(ctx, func(ctx context.Context) error {
		err := service.ruleStore.UpdateAlertRules(ctx, []models.UpdateRule{
			{
				Existing: storedRule,
				New:      rule,
			},
		})
		if err != nil {
			return err
		}
		return service.provenanceStore.SetProvenance(ctx, &rule, rule.OrgID, provenance)
	})
	if err != nil {
		return models.AlertRule{}, err
	}
	return rule, err
}

func (service *AlertRuleService) DeleteAlertRule(ctx context.Context, user identity.Requester, orgID int64, ruleUID string, provenance models.Provenance) error {
	rule := &models.AlertRule{
		OrgID: orgID,
		UID:   ruleUID,
	}
	// check that provenance is not changed in an invalid way
	storedProvenance, err := service.provenanceStore.GetProvenance(ctx, rule, rule.OrgID)
	if err != nil {
		return err
	}
	if storedProvenance != provenance && storedProvenance != models.ProvenanceNone {
		return fmt.Errorf("cannot delete with provided provenance '%s', needs '%s'", provenance, storedProvenance)
	}

	if can, err := service.authz.CanWriteAllRules(ctx, user); !can || err != nil {
		if err != nil {
			return err
		}
		delta, err := store.CalculateRuleDelete(ctx, service.ruleStore, rule.GetKey())
		if err != nil {
			return err
		}
		if err = service.authz.AuthorizeRuleChanges(ctx, user, delta); err != nil {
			return err
		}
	}

	return service.xact.InTransaction(ctx, func(ctx context.Context) error {
		return service.deleteRules(ctx, orgID, rule)
	})
}

// checkLimitsTransactionCtx checks whether the current transaction (as identified by the ctx) breaches configured alert rule limits.
func (service *AlertRuleService) checkLimitsTransactionCtx(ctx context.Context, orgID, userID int64) error {
	limitReached, err := service.quotas.CheckQuotaReached(ctx, models.QuotaTargetSrv, &quota.ScopeParameters{
		OrgID:  orgID,
		UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("failed to check alert rule quota: %w", err)
	}
	if limitReached {
		return models.ErrQuotaReached
	}
	return nil
}

// deleteRules deletes a set of target rules and associated data, while checking for database consistency.
func (service *AlertRuleService) deleteRules(ctx context.Context, orgID int64, targets ...*models.AlertRule) error {
	uids := make([]string, 0, len(targets))
	for _, tgt := range targets {
		if tgt != nil {
			uids = append(uids, tgt.UID)
		}
	}
	if err := service.ruleStore.DeleteAlertRulesByUID(ctx, orgID, uids...); err != nil {
		return err
	}
	for _, uid := range uids {
		if err := service.provenanceStore.DeleteProvenance(ctx, &models.AlertRule{UID: uid}, orgID); err != nil {
			// We failed to clean up the record, but this doesn't break things. Log it and move on.
			service.log.Warn("Failed to delete provenance record for rule: %w", err)
		}
	}
	return nil
}

// GetAlertRuleGroupWithFolderTitle returns the alert rule group with folder title.
func (service *AlertRuleService) GetAlertRuleGroupWithFolderTitle(ctx context.Context, user identity.Requester, orgID int64, namespaceUID, group string) (models.AlertRuleGroupWithFolderTitle, error) {
	ruleList, err := service.GetRuleGroup(ctx, user, orgID, namespaceUID, group)
	if err != nil {
		return models.AlertRuleGroupWithFolderTitle{}, err
	}

	dq := dashboards.GetDashboardQuery{
		OrgID: orgID,
		UID:   namespaceUID,
	}
	dash, err := service.dashboardService.GetDashboard(ctx, &dq)
	if err != nil {
		return models.AlertRuleGroupWithFolderTitle{}, err
	}

	res := models.NewAlertRuleGroupWithFolderTitle(ruleList.Rules[0].GetGroupKey(), ruleList.Rules, dash.Title)
	return res, nil
}

// GetAlertGroupsWithFolderTitle returns all groups with folder title in the folders identified by folderUID that have at least one alert. If argument folderUIDs is nil or empty - returns groups in all folders.
func (service *AlertRuleService) GetAlertGroupsWithFolderTitle(ctx context.Context, user identity.Requester, orgID int64, folderUIDs []string) ([]models.AlertRuleGroupWithFolderTitle, error) {
	q := models.ListAlertRulesQuery{
		OrgID: orgID,
	}

	if len(folderUIDs) > 0 {
		q.NamespaceUIDs = folderUIDs
	}

	ruleList, err := service.ruleStore.ListAlertRules(ctx, &q)
	if err != nil {
		return nil, err
	}
	groups := models.GroupByAlertRuleGroupKey(ruleList)
	if can, err := service.authz.CanReadAllRules(ctx, user); !can || err != nil {
		if err != nil {
			return nil, err
		}
		for key, group := range groups {
			if err := service.authz.AuthorizeAccessToRuleGroup(ctx, user, group); err != nil {
				if accesscontrol.IsAuthorizationError(err) {
					delete(groups, key)
					continue
				}
				return nil, err
			}
		}
	}

	namespaces := make(map[string][]*models.AlertRuleGroupKey)
	for groupKey := range groups {
		namespaces[groupKey.NamespaceUID] = append(namespaces[groupKey.NamespaceUID], &groupKey)
	}

	if len(namespaces) == 0 {
		return []models.AlertRuleGroupWithFolderTitle{}, nil
	}

	dq := dashboards.GetDashboardsQuery{
		DashboardUIDs: nil,
	}
	for uid := range namespaces {
		dq.DashboardUIDs = append(dq.DashboardUIDs, uid)
	}

	// We need folder titles for the provisioning file format. We do it this way instead of using GetUserVisibleNamespaces to avoid folder:read permissions that should not apply to those with alert.provisioning:read.
	dashes, err := service.dashboardService.GetDashboards(ctx, &dq)
	if err != nil {
		return nil, err
	}
	folderUidToTitle := make(map[string]string)
	for _, dash := range dashes {
		folderUidToTitle[dash.UID] = dash.Title
	}

	result := make([]models.AlertRuleGroupWithFolderTitle, 0)
	for groupKey, rules := range groups {
		title, ok := folderUidToTitle[groupKey.NamespaceUID]
		if !ok {
			return nil, fmt.Errorf("cannot find title for folder with uid '%s'", groupKey.NamespaceUID)
		}
		result = append(result, models.NewAlertRuleGroupWithFolderTitleFromRulesGroup(groupKey, rules, title))
	}

	// Return results in a stable manner.
	models.SortAlertRuleGroupWithFolderTitle(result)
	return result, nil
}

// syncRuleGroupFields synchronizes calculated fields across multiple rules in a group.
func syncGroupRuleFields(group *models.AlertRuleGroup, orgID int64) *models.AlertRuleGroup {
	for i := range group.Rules {
		group.Rules[i].IntervalSeconds = group.Interval
		group.Rules[i].RuleGroup = group.Title
		group.Rules[i].NamespaceUID = group.FolderUID
		group.Rules[i].OrgID = orgID
	}
	return group
}

func withoutNilAlertRules(ptrs []*models.AlertRule) []models.AlertRule {
	result := make([]models.AlertRule, 0, len(ptrs))
	for _, ptr := range ptrs {
		if ptr != nil {
			result = append(result, *ptr)
		}
	}
	return result
}

func (service *AlertRuleService) checkGroupLimits(group models.AlertRuleGroup) error {
	if service.rulesPerRuleGroupLimit > 0 && int64(len(group.Rules)) > service.rulesPerRuleGroupLimit {
		service.log.Warn("Large rule group was edited. Large groups are discouraged and may be rejected in the future.",
			"limit", service.rulesPerRuleGroupLimit,
			"actual", len(group.Rules),
			"group", group.Title,
		)
	}

	return nil
}
