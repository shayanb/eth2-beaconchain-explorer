package services

import (
	"eth2-exporter/db"
	ethclients "eth2-exporter/ethClients"
	"eth2-exporter/mail"
	"eth2-exporter/metrics"
	"eth2-exporter/notify"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"firebase.google.com/go/messaging"
	"github.com/jmoiron/sqlx"
)

func notificationsSender() {
	for {
		// check if the explorer is not too far behind, if we set this value to close (10m) it could potentially never send any notifications
		// if IsSyncing() {
		if time.Now().Add(time.Minute * -20).After(utils.EpochToTime(LatestEpoch())) {
			logger.Info("skipping notifications because the explorer is syncing")
			time.Sleep(time.Second * 60)
			continue
		}
		start := time.Now()

		// Network DB Notifications (validator related)
		notifications := collectNotifications()
		sendNotifications(notifications, db.DB)

		// Network DB Notifications (validator related)
		if utils.Config.Notifications.UserDBNotifications {
			userNotifications := collectUserDbNotifications()
			sendNotifications(userNotifications, db.FrontendDB)
		}

		logger.WithField("notifications", len(notifications)).WithField("duration", time.Since(start)).Info("notifications completed")
		metrics.TaskDuration.WithLabelValues("service_notifications").Observe(time.Since(start).Seconds())
		time.Sleep(time.Second * 120)
	}
}

func collectNotifications() map[uint64]map[types.EventName][]types.Notification {
	notificationsByUserID := map[uint64]map[types.EventName][]types.Notification{}
	var err error
	if utils.Config.Notifications.ValidatorBalanceDecreasedNotificationsEnabled {
		err = collectValidatorBalanceDecreasedNotifications(notificationsByUserID)
		if err != nil {
			logger.Errorf("error collecting validator_balance_decreased notifications: %v", err)
		}
	}
	err = collectValidatorGotSlashedNotifications(notificationsByUserID)
	if err != nil {
		logger.Errorf("error collecting validator_got_slashed notifications: %v", err)
	}

	// executed Proposals
	err = collectBlockProposalNotifications(notificationsByUserID, 1, types.ValidatorExecutedProposalEventName)
	if err != nil {
		logger.Errorf("error collecting validator_proposal_submitted notifications: %v", err)
	}

	// Missed proposals
	err = collectBlockProposalNotifications(notificationsByUserID, 2, types.ValidatorMissedProposalEventName)
	if err != nil {
		logger.Errorf("error collecting validator_proposal_missed notifications: %v", err)
	}

	// Missed attestations
	err = collectAttestationNotifications(notificationsByUserID, 0, types.ValidatorMissedAttestationEventName)
	if err != nil {
		logger.Errorf("error collecting validator_attestation_missed notifications: %v", err)
	}

	// New ETH clients
	err = collectEthClientNotifications(notificationsByUserID, types.EthClientUpdateEventName)
	if err != nil {
		logger.Errorf("error collecting Eth client notifications: %v", err)
	}

	//Tax Report
	err = collectTaxReportNotificationNotifications(notificationsByUserID, types.TaxReportEventName)
	if err != nil {
		logger.Errorf("error collecting tax report notifications: %v", err)
	}

	return notificationsByUserID
}

func collectUserDbNotifications() map[uint64]map[types.EventName][]types.Notification {
	notificationsByUserID := map[uint64]map[types.EventName][]types.Notification{}
	var err error

	// Monitoring (premium): machine offline
	err = collectMonitoringMachineOffline(notificationsByUserID)
	if err != nil {
		logger.Errorf("error collecting Eth client offline notifications: %v", err)
	}

	// Monitoring (premium): disk full warnings
	err = collectMonitoringMachineDiskAlmostFull(notificationsByUserID)
	if err != nil {
		logger.Errorf("error collecting Eth client disk full notifications: %v", err)
	}

	// Monitoring (premium): cpu load
	err = collectMonitoringMachineCPULoad(notificationsByUserID)
	if err != nil {
		logger.Errorf("error collecting Eth client cpu notifications: %v", err)
	}

	// Monitoring (premium): ram
	err = collectMonitoringMachineMemoryUsage(notificationsByUserID)
	if err != nil {
		logger.Errorf("error collecting Eth client memory notifications: %v", err)
	}

	return notificationsByUserID
}

func sendNotifications(notificationsByUserID map[uint64]map[types.EventName][]types.Notification, useDB *sqlx.DB) {
	sendEmailNotifications(notificationsByUserID, useDB)
	sendPushNotifications(notificationsByUserID, useDB)
	// sendWebhookNotifications(notificationsByUserID)
}

func getNetwork() string {
	domainParts := strings.Split(utils.Config.Frontend.SiteDomain, ".")
	if len(domainParts) >= 3 {
		return fmt.Sprintf("%s: ", strings.Title(domainParts[0]))
	}
	return ""
}

func sendPushNotifications(notificationsByUserID map[uint64]map[types.EventName][]types.Notification, useDB *sqlx.DB) {
	userIDs := []uint64{}
	for userID := range notificationsByUserID {
		userIDs = append(userIDs, userID)
	}

	tokensByUserID, err := db.GetUserPushTokenByIds(userIDs)
	if err != nil {
		logger.Errorf("error when sending push-notificaitons: could not get tokens: %v", err)
		return
	}

	for userID, userNotifications := range notificationsByUserID {
		userTokens, exists := tokensByUserID[userID]
		if !exists {
			continue
		}

		go func(userTokens []string, userNotifications map[types.EventName][]types.Notification) {
			var batch []*messaging.Message
			sentSubsByEpoch := map[uint64][]uint64{}

			for _, ns := range userNotifications {
				for _, n := range ns {
					for _, userToken := range userTokens {

						notification := new(messaging.Notification)
						notification.Title = fmt.Sprintf("%s%s", getNetwork(), n.GetTitle())
						notification.Body = n.GetInfo(false)

						message := new(messaging.Message)
						message.Notification = notification
						message.Token = userToken

						message.APNS = new(messaging.APNSConfig)
						message.APNS.Payload = new(messaging.APNSPayload)
						message.APNS.Payload.Aps = new(messaging.Aps)
						message.APNS.Payload.Aps.Sound = "default"

						batch = append(batch, message)
					}

					e := n.GetEpoch()
					if _, exists := sentSubsByEpoch[e]; !exists {
						sentSubsByEpoch[e] = []uint64{n.GetSubscriptionID()}
					} else {
						sentSubsByEpoch[e] = append(sentSubsByEpoch[e], n.GetSubscriptionID())
					}
				}
			}

			_, err := notify.SendPushBatch(batch)
			if err != nil {
				logger.Errorf("firebase batch job failed: %v", err)
				return
			}

			for epoch, subIDs := range sentSubsByEpoch {
				err = db.UpdateSubscriptionsLastSent(subIDs, time.Now(), epoch, useDB)
				if err != nil {
					logger.Errorf("error updating sent-time of sent notifications: %v", err)
				}
			}
		}(userTokens, userNotifications)
	}

}

func sendEmailNotifications(notificationsByUserID map[uint64]map[types.EventName][]types.Notification, useDB *sqlx.DB) {
	userIDs := []uint64{}
	for userID := range notificationsByUserID {
		userIDs = append(userIDs, userID)
	}
	emailsByUserID, err := db.GetUserEmailsByIds(userIDs)
	if err != nil {
		logger.Errorf("error when sending eamil-notificaitons: could not get emails: %v", err)
		return
	}

	for userID, userNotifications := range notificationsByUserID {
		userEmail, exists := emailsByUserID[userID]
		if !exists {
			logger.Errorf("error when sending email-notification: could not find email for user %v", userID)
			continue
		}
		go func(userEmail string, userNotifications map[types.EventName][]types.Notification) {
			sentSubsByEpoch := map[uint64][]uint64{}
			subject := fmt.Sprintf("%s: Notification", utils.Config.Frontend.SiteDomain)
			msg := ""
			attachments := []types.EmailAttachment{}
			for event, ns := range userNotifications {
				if len(msg) > 0 {
					msg += "\n"
				}
				event_title := event
				if event == types.TaxReportEventName {
					event_title = "income_history"
				}
				msg += fmt.Sprintf("%s\n====\n\n", event_title)
				for _, n := range ns {
					msg += fmt.Sprintf("%s\n", n.GetInfo(true))
					e := n.GetEpoch()
					if _, exists := sentSubsByEpoch[e]; !exists {
						sentSubsByEpoch[e] = []uint64{n.GetSubscriptionID()}
					} else {
						sentSubsByEpoch[e] = append(sentSubsByEpoch[e], n.GetSubscriptionID())
					}
					if att := n.GetEmailAttachment(); att != nil {
						attachments = append(attachments, *att)
					}

				}
				if event == "validator_balance_decreased" {
					msg += "\nYou will not receive any further balance decrease mails for these validators until the balance of a validator is increasing again.\n"
				}
			}
			msg += fmt.Sprintf("\nBest regards\n\n%s", utils.Config.Frontend.SiteDomain)

			err := mail.SendMailRateLimited(userEmail, subject, msg, attachments)
			if err != nil {
				logger.Errorf("error sending notification-email: %v", err)
				return
			}

			for epoch, subIDs := range sentSubsByEpoch {
				err = db.UpdateSubscriptionsLastSent(subIDs, time.Now(), epoch, useDB)
				if err != nil {
					logger.Errorf("error updating sent-time of sent notifications: %v", err)
				}
			}
		}(userEmail, userNotifications)
	}
}

type validatorBalanceDecreasedNotification struct {
	ValidatorIndex     uint64
	ValidatorPublicKey string
	StartEpoch         uint64
	EndEpoch           uint64
	StartBalance       uint64
	EndBalance         uint64
	SubscriptionID     uint64
	EventFilter        string
}

func (n *validatorBalanceDecreasedNotification) GetEmailAttachment() *types.EmailAttachment {
	return nil
}

func (n *validatorBalanceDecreasedNotification) GetSubscriptionID() uint64 {
	return n.SubscriptionID
}

func (n *validatorBalanceDecreasedNotification) GetEpoch() uint64 {
	return n.StartEpoch
}

func (n *validatorBalanceDecreasedNotification) GetEventName() types.EventName {
	return types.ValidatorBalanceDecreasedEventName
}

func (n *validatorBalanceDecreasedNotification) GetInfo(includeUrl bool) string {
	balance := float64(n.EndBalance) / 1e9
	diff := float64(n.StartBalance-n.EndBalance) / 1e9

	generalPart := fmt.Sprintf(`The balance of validator %[1]v decreased for 3 consecutive epochs by %.9[2]f ETH to %.9[3]f ETH from epoch %[4]v to epoch %[5]v.`, n.ValidatorIndex, diff, balance, n.StartEpoch, n.EndEpoch)
	if includeUrl {
		return generalPart + getUrlPart(n.ValidatorIndex)
	}
	return generalPart
}

func (n *validatorBalanceDecreasedNotification) GetTitle() string {
	return "Validator Balance Decreased"
}

func (n *validatorBalanceDecreasedNotification) GetEventFilter() string {
	return n.EventFilter
}

func getUrlPart(validatorIndex uint64) string {
	return fmt.Sprintf(` For more information visit: https://%[2]s/validator/%[1]v`, validatorIndex, utils.Config.Frontend.SiteDomain)
}

// collectValidatorBalanceDecreasedNotifications finds all validators whose balance decreased for 3 consecutive epochs
// and creates notifications for all subscriptions which have not been notified about the validator since the last time its balance increased.
// It looks 10 epochs back for when the balance increased the last time, this means if the explorer is not running for 10 epochs it is possible
// that no new notification is sent even if there was a balance-increase.
func collectValidatorBalanceDecreasedNotifications(notificationsByUserID map[uint64]map[types.EventName][]types.Notification) error {
	latestEpoch := LatestEpoch()
	if latestEpoch < 3 {
		return nil
	}

	var dbResult []struct {
		SubscriptionID uint64 `db:"id"`
		UserID         uint64 `db:"user_id"`
		ValidatorIndex uint64 `db:"validatorindex"`
		StartBalance   uint64 `db:"startbalance"`
		EndBalance     uint64 `db:"endbalance"`
		EventFilter    string `db:"pubkey"`
	}

	err := db.DB.Select(&dbResult, `
		SELECT id, user_id, validatorindex, startbalance, endbalance, a.pubkey AS pubkey FROM (
			SELECT 
				us.id, 
				us.user_id, 
				v.validatorindex,
				v.pubkeyhex AS pubkey, 
				vb0.balance AS endbalance, 
				vb3.balance AS startbalance, 
				us.last_sent_epoch,
				(SELECT MAX(epoch) FROM (
					SELECT epoch, balance-LAG(balance) OVER (ORDER BY epoch) AS diff
					FROM validator_balances_recent 
					WHERE validatorindex = v.validatorindex AND epoch > us.last_sent_epoch AND epoch > $2 - 10
				) b WHERE diff > 0) AS lastbalanceincreaseepoch
			FROM users_subscriptions us
			INNER JOIN validators v ON v.pubkeyhex = us.event_filter
			INNER JOIN validator_balances_recent vb0 ON v.validatorindex = vb0.validatorindex AND vb0.epoch = $2
			INNER JOIN validator_balances_recent vb1 ON v.validatorindex = vb1.validatorindex AND vb1.epoch = $2 - 1 AND vb1.balance > vb0.balance
			INNER JOIN validator_balances_recent vb2 ON v.validatorindex = vb2.validatorindex AND vb2.epoch = $2 - 2 AND vb2.balance > vb1.balance
			INNER JOIN validator_balances_recent vb3 ON v.validatorindex = vb3.validatorindex AND vb3.epoch = $2 - 3 AND vb3.balance > vb2.balance
			WHERE us.event_name = $1 AND us.created_epoch <= $2
		) a WHERE lastbalanceincreaseepoch IS NOT NULL OR last_sent_epoch IS NULL`,
		types.ValidatorBalanceDecreasedEventName, latestEpoch)
	if err != nil {
		return err
	}

	for _, r := range dbResult {
		n := &validatorBalanceDecreasedNotification{
			SubscriptionID: r.SubscriptionID,
			ValidatorIndex: r.ValidatorIndex,
			StartEpoch:     latestEpoch - 3,
			EndEpoch:       latestEpoch,
			StartBalance:   r.StartBalance,
			EndBalance:     r.EndBalance,
			EventFilter:    r.EventFilter,
		}

		if _, exists := notificationsByUserID[r.UserID]; !exists {
			notificationsByUserID[r.UserID] = map[types.EventName][]types.Notification{}
		}
		if _, exists := notificationsByUserID[r.UserID][n.GetEventName()]; !exists {
			notificationsByUserID[r.UserID][n.GetEventName()] = []types.Notification{}
		}
		notificationsByUserID[r.UserID][n.GetEventName()] = append(notificationsByUserID[r.UserID][n.GetEventName()], n)
	}

	return nil
}

func collectBlockProposalNotifications(notificationsByUserID map[uint64]map[types.EventName][]types.Notification, status uint64, eventName types.EventName) error {
	latestEpoch := LatestEpoch()

	var dbResult []struct {
		SubscriptionID uint64 `db:"id"`
		UserID         uint64 `db:"user_id"`
		ValidatorIndex uint64 `db:"validatorindex"`
		Epoch          uint64 `db:"epoch"`
		Status         uint64 `db:"status"`
		EventFilter    string `db:"pubkey"`
	}

	err := db.DB.Select(&dbResult, `
			SELECT 
				us.id, 
				us.user_id, 
				v.validatorindex, 
				pa.epoch,
				pa.status,
				ENCODE(v.pubkey::bytea, 'hex') AS pubkey
			FROM users_subscriptions us
			INNER JOIN validators v ON ENCODE(v.pubkey, 'hex') = us.event_filter
			INNER JOIN proposal_assignments pa ON v.validatorindex = pa.validatorindex AND pa.epoch >= ($2 - 5) 
			WHERE us.event_name = $1 AND pa.status = $3 AND us.created_epoch <= $2 AND pa.epoch >= ($2 - 5) AND (us.last_sent_epoch < pa.epoch OR us.last_sent_epoch IS NULL)`,
		eventName, latestEpoch, status)
	if err != nil {
		return err
	}

	for _, r := range dbResult {

		n := &validatorProposalNotification{
			SubscriptionID: r.SubscriptionID,
			ValidatorIndex: r.ValidatorIndex,
			Epoch:          r.Epoch,
			Status:         r.Status,
			EventName:      eventName,
			EventFilter:    r.EventFilter,
		}

		if _, exists := notificationsByUserID[r.UserID]; !exists {
			notificationsByUserID[r.UserID] = map[types.EventName][]types.Notification{}
		}
		if _, exists := notificationsByUserID[r.UserID][n.GetEventName()]; !exists {
			notificationsByUserID[r.UserID][n.GetEventName()] = []types.Notification{}
		}
		notificationsByUserID[r.UserID][n.GetEventName()] = append(notificationsByUserID[r.UserID][n.GetEventName()], n)
	}

	return nil
}

type validatorProposalNotification struct {
	SubscriptionID     uint64
	ValidatorIndex     uint64
	ValidatorPublicKey string
	Epoch              uint64
	Status             uint64 // * Can be 0 = scheduled, 1 executed, 2 missed */
	EventName          types.EventName
	EventFilter        string
}

func (n *validatorProposalNotification) GetEmailAttachment() *types.EmailAttachment {
	return nil
}

func (n *validatorProposalNotification) GetSubscriptionID() uint64 {
	return n.SubscriptionID
}

func (n *validatorProposalNotification) GetEpoch() uint64 {
	return n.Epoch
}

func (n *validatorProposalNotification) GetEventName() types.EventName {
	return n.EventName
}

func (n *validatorProposalNotification) GetInfo(includeUrl bool) string {
	var generalPart = ""
	switch n.Status {
	case 0:
		generalPart = fmt.Sprintf(`New scheduled block proposal for Validator %[1]v.`, n.ValidatorIndex)
	case 1:
		generalPart = fmt.Sprintf(`Validator %[1]v proposed a new block.`, n.ValidatorIndex)
	case 2:
		generalPart = fmt.Sprintf(`Validator %[1]v missed a block proposal.`, n.ValidatorIndex)
	}

	if includeUrl {
		return generalPart + getUrlPart(n.ValidatorIndex)
	}
	return generalPart
}

func (n *validatorProposalNotification) GetTitle() string {
	switch n.Status {
	case 0:
		return "Block Proposal Scheduled"
	case 1:
		return "New Block Proposal"
	case 2:
		return "Block Proposal Missed"
	}
	return "-"
}

func (n *validatorProposalNotification) GetEventFilter() string {
	return n.EventFilter
}

func collectAttestationNotifications(notificationsByUserID map[uint64]map[types.EventName][]types.Notification, status uint64, eventName types.EventName) error {
	latestEpoch := LatestEpoch()
	latestSlot := LatestSlot()

	var dbResult []struct {
		SubscriptionID uint64 `db:"id"`
		UserID         uint64 `db:"user_id"`
		ValidatorIndex uint64 `db:"validatorindex"`
		Epoch          uint64 `db:"epoch"`
		Status         uint64 `db:"status"`
		Slot           uint64 `db:"attesterslot"`
		InclusionSlot  uint64 `db:"inclusionslot"`
		EventFilter    string `db:"pubkey"`
	}

	err := db.DB.Select(&dbResult, `
			SELECT 
				us.id, 
				us.user_id, 
				v.validatorindex, 
				aa.epoch,
				aa.status,
				aa.attesterslot,
				aa.inclusionslot,
				ENCODE(v.pubkey::bytea, 'hex') AS pubkey
			FROM users_subscriptions us
			INNER JOIN validators v ON ENCODE(v.pubkey, 'hex') = us.event_filter
			INNER JOIN attestation_assignments_p aa ON v.validatorindex = aa.validatorindex AND aa.epoch >= ($2 - 3)  AND aa.week >= ($2 - 3) / 1575
			WHERE us.event_name = $1 AND aa.status = $3 AND us.created_epoch <= $2 AND aa.epoch >= ($2 - 3)
			AND (us.last_sent_epoch < ($2 - 6) OR us.last_sent_epoch IS NULL)
			AND aa.inclusionslot = 0 AND aa.attesterslot < ($4 - 32)
			`,
		eventName, latestEpoch, status, latestSlot)
	if err != nil {
		return err
	}

	for _, r := range dbResult {

		n := &validatorAttestationNotification{
			SubscriptionID: r.SubscriptionID,
			ValidatorIndex: r.ValidatorIndex,
			Epoch:          r.Epoch,
			Status:         r.Status,
			EventName:      eventName,
			Slot:           r.Slot,
			InclusionSlot:  r.InclusionSlot,
			EventFilter:    r.EventFilter,
		}

		if _, exists := notificationsByUserID[r.UserID]; !exists {
			notificationsByUserID[r.UserID] = map[types.EventName][]types.Notification{}
		}
		if _, exists := notificationsByUserID[r.UserID][n.GetEventName()]; !exists {
			notificationsByUserID[r.UserID][n.GetEventName()] = []types.Notification{}
		}
		notificationsByUserID[r.UserID][n.GetEventName()] = append(notificationsByUserID[r.UserID][n.GetEventName()], n)
	}

	return nil
}

type validatorAttestationNotification struct {
	SubscriptionID     uint64
	ValidatorIndex     uint64
	ValidatorPublicKey string
	Epoch              uint64
	Status             uint64 // * Can be 0 = scheduled | missed, 1 executed
	EventName          types.EventName
	Slot               uint64
	InclusionSlot      uint64
	EventFilter        string
}

func (n *validatorAttestationNotification) GetEmailAttachment() *types.EmailAttachment {
	return nil
}

func (n *validatorAttestationNotification) GetSubscriptionID() uint64 {
	return n.SubscriptionID
}

func (n *validatorAttestationNotification) GetEpoch() uint64 {
	return n.Epoch
}

func (n *validatorAttestationNotification) GetEventName() types.EventName {
	return n.EventName
}

func (n *validatorAttestationNotification) GetInfo(includeUrl bool) string {
	var generalPart = ""
	switch n.Status {
	case 0:
		generalPart = fmt.Sprintf(`Validator %[1]v missed an attestation at slot %[2]v.`, n.ValidatorIndex, n.Slot)
		//generalPart = fmt.Sprintf(`New scheduled attestation for Validator %[1]v at slot %[2]v.`, n.ValidatorIndex, n.Slot)
	case 1:
		generalPart = fmt.Sprintf(`Validator %[1]v submitted a successfull attestation for slot %[2]v.`, n.ValidatorIndex, n.Slot)
	}

	if includeUrl {
		return generalPart + getUrlPart(n.ValidatorIndex)
	}
	return generalPart
}

func (n *validatorAttestationNotification) GetTitle() string {
	switch n.Status {
	case 0:
		return "Attestation Missed"
	case 1:
		return "Attestation Submitted"
	}
	return "-"
}

func (n *validatorAttestationNotification) GetEventFilter() string {
	return n.EventFilter
}

type validatorGotSlashedNotification struct {
	SubscriptionID uint64
	ValidatorIndex uint64
	Epoch          uint64
	Slasher        uint64
	Reason         string
	EventFilter    string
}

func (n *validatorGotSlashedNotification) GetEmailAttachment() *types.EmailAttachment {
	return nil
}

func (n *validatorGotSlashedNotification) GetSubscriptionID() uint64 {
	return n.SubscriptionID
}

func (n *validatorGotSlashedNotification) GetEpoch() uint64 {
	return n.Epoch
}

func (n *validatorGotSlashedNotification) GetEventName() types.EventName {
	return types.ValidatorGotSlashedEventName
}

func (n *validatorGotSlashedNotification) GetInfo(includeUrl bool) string {
	generalPart := fmt.Sprintf(`Validator %[1]v has been slashed at epoch %[2]v by validator %[3]v for %[4]s.`, n.ValidatorIndex, n.Epoch, n.Slasher, n.Reason)
	if includeUrl {
		return generalPart + getUrlPart(n.ValidatorIndex)
	}
	return generalPart
}

func (n *validatorGotSlashedNotification) GetTitle() string {
	return "Validator got Slashed"
}

func (n *validatorGotSlashedNotification) GetEventFilter() string {
	return n.EventFilter
}

func collectValidatorGotSlashedNotifications(notificationsByUserID map[uint64]map[types.EventName][]types.Notification) error {
	latestEpoch := LatestEpoch()
	if latestEpoch == 0 {
		return nil
	}

	var dbResult []struct {
		SubscriptionID uint64 `db:"id"`
		UserID         uint64 `db:"user_id"`
		ValidatorIndex uint64 `db:"validatorindex"`
		Slasher        uint64 `db:"slasher"`
		Epoch          uint64 `db:"epoch"`
		Reason         string `db:"reason"`
		EventFilter    string `db:"pubkey"`
	}

	err := db.DB.Select(&dbResult, `
		WITH
			slashings AS (
				SELECT DISTINCT ON (slashedvalidator) * FROM (
					SELECT
						blocks.slot, 
						blocks.epoch, 
						blocks.proposer AS slasher, 
						UNNEST(ARRAY(
							SELECT UNNEST(attestation1_indices)
								INTERSECT
							SELECT UNNEST(attestation2_indices)
						)) AS slashedvalidator, 
						'Attestation Violation' AS reason
					FROM blocks_attesterslashings 
					LEFT JOIN blocks ON blocks_attesterslashings.block_slot = blocks.slot
					WHERE blocks.status = '1'
					UNION ALL
						SELECT
							blocks.slot, 
							blocks.epoch, 
							blocks.proposer AS slasher, 
							blocks_proposerslashings.proposerindex AS slashedvalidator,
							'Proposer Violation' AS reason 
						FROM blocks_proposerslashings
						LEFT JOIN blocks ON blocks_proposerslashings.block_slot = blocks.slot
						WHERE blocks.status = '1'
				) a
				ORDER BY slashedvalidator, slot
			)
		SELECT us.id, us.user_id, v.validatorindex, s.slasher, s.epoch, s.reason, ENCODE(v.pubkey::bytea, 'hex') AS pubkey
		FROM users_subscriptions us
		INNER JOIN validators v ON ENCODE(v.pubkey, 'hex') = us.event_filter
		INNER JOIN slashings s ON s.slashedvalidator = v.validatorindex
		WHERE us.event_name = $1 AND us.last_sent_epoch IS NULL AND us.created_epoch < s.epoch`,
		types.ValidatorGotSlashedEventName)
	if err != nil {
		return err
	}

	for _, r := range dbResult {
		n := &validatorGotSlashedNotification{
			SubscriptionID: r.SubscriptionID,
			ValidatorIndex: r.ValidatorIndex,
			Slasher:        r.Slasher,
			Epoch:          r.Epoch,
			Reason:         r.Reason,
			EventFilter:    r.EventFilter,
		}
		if _, exists := notificationsByUserID[r.UserID]; !exists {
			notificationsByUserID[r.UserID] = map[types.EventName][]types.Notification{}
		}
		if _, exists := notificationsByUserID[r.UserID][n.GetEventName()]; !exists {
			notificationsByUserID[r.UserID][n.GetEventName()] = []types.Notification{}
		}
		notificationsByUserID[r.UserID][n.GetEventName()] = append(notificationsByUserID[r.UserID][n.GetEventName()], n)
	}

	return nil
}

type ethClientNotification struct {
	SubscriptionID uint64
	UserID         uint64
	Epoch          uint64
	EthClient      string
	EventFilter    string
}

func (n *ethClientNotification) GetEmailAttachment() *types.EmailAttachment {
	return nil
}

func (n *ethClientNotification) GetSubscriptionID() uint64 {
	return n.SubscriptionID
}

func (n *ethClientNotification) GetEpoch() uint64 {
	return n.Epoch
}

func (n *ethClientNotification) GetEventName() types.EventName {
	return types.EthClientUpdateEventName
}

func (n *ethClientNotification) GetInfo(includeUrl bool) string {
	generalPart := fmt.Sprintf(`A new version for %s is available.`, n.EthClient)
	if includeUrl {
		url := ""
		switch n.EthClient {
		case "Geth":
			url = "https://github.com/ethereum/go-ethereum/releases"
		case "Nethermind":
			url = "https://github.com/NethermindEth/nethermind/releases"
		case "OpenEthereum":
			url = "https://github.com/openethereum/openethereum/releases"
		case "Teku":
			url = "https://github.com/ConsenSys/teku/releases"
		case "Prysm":
			url = "https://github.com/prysmaticlabs/prysm/releases"
		case "Nimbus":
			url = "https://github.com/status-im/nimbus-eth2/releases"
		case "Lighthouse":
			url = "https://github.com/sigp/lighthouse/releases"
		default:
			url = "https://beaconcha.in/ethClients"
		}

		return generalPart + " " + url
	}
	return generalPart
}

func (n *ethClientNotification) GetTitle() string {
	return fmt.Sprintf("New %s update", n.EthClient)
}

func (n *ethClientNotification) GetEventFilter() string {
	return n.EventFilter
}

func collectEthClientNotifications(notificationsByUserID map[uint64]map[types.EventName][]types.Notification, eventName types.EventName) error {
	updatedClients := ethclients.GetUpdatedClients() //only check if there are new updates
	for _, client := range updatedClients {
		var dbResult []struct {
			SubscriptionID uint64 `db:"id"`
			UserID         uint64 `db:"user_id"`
			Epoch          uint64 `db:"created_epoch"`
			EventFilter    string `db:"event_filter"`
		}

		err := db.DB.Select(&dbResult, `
			SELECT us.id, us.user_id, us.created_epoch, us.event_filter
			FROM users_subscriptions AS us
			WHERE 
				us.event_name=$1 
			AND 
				us.event_filter=$2 
			AND 
				((us.last_sent_ts <= NOW() - INTERVAL '2 DAY' AND TO_TIMESTAMP($3) > us.last_sent_ts) OR us.last_sent_ts IS NULL)
			`,
			eventName, strings.ToLower(client.Name), client.Date.Unix()) // was last notification sent 2 days ago for this client

		if err != nil {
			return err
		}

		for _, r := range dbResult {
			n := &ethClientNotification{
				SubscriptionID: r.SubscriptionID,
				UserID:         r.UserID,
				Epoch:          r.Epoch,
				EventFilter:    r.EventFilter,
				EthClient:      client.Name,
			}
			if _, exists := notificationsByUserID[r.UserID]; !exists {
				notificationsByUserID[r.UserID] = map[types.EventName][]types.Notification{}
			}
			if _, exists := notificationsByUserID[r.UserID][n.GetEventName()]; !exists {
				notificationsByUserID[r.UserID][n.GetEventName()] = []types.Notification{}
			}
			notificationsByUserID[r.UserID][n.GetEventName()] = append(notificationsByUserID[r.UserID][n.GetEventName()], n)
		}
	}
	return nil
}

func collectMonitoringMachineOffline(notificationsByUserID map[uint64]map[types.EventName][]types.Notification) error {
	return collectMonitoringMachine(notificationsByUserID, types.MonitoringMachineOfflineEventName,
		`SELECT 
		us.user_id,
		max(us.id) as id,
		machine  
	FROM users_subscriptions us
	JOIN (
		SELECT max(id) as id, user_id, machine, max(created_trunc) as created_trunc from stats_meta_p 
		WHERE day >= $3 
		group by user_id, machine
	) v on v.user_id = us.user_id 
	WHERE us.event_name = $1 AND us.created_epoch <= $2 
	AND us.event_filter = v.machine 
	AND (us.last_sent_epoch < ($2 - 120) OR us.last_sent_epoch IS NULL)
	AND v.created_trunc < now() - interval '4 minutes' AND v.created_trunc > now() - interval '1 hours'
	group by us.user_id, machine
	`)
}

func collectMonitoringMachineDiskAlmostFull(notificationsByUserID map[uint64]map[types.EventName][]types.Notification) error {
	return collectMonitoringMachine(notificationsByUserID, types.MonitoringMachineDiskAlmostFullEventName,
		`SELECT 
			us.user_id,
			max(us.id) as id,
			machine 
		FROM users_subscriptions us 
		INNER JOIN stats_meta_p v ON us.user_id = v.user_id
		INNER JOIN stats_system sy ON v.id = sy.meta_id
		WHERE us.event_name = $1 AND us.created_epoch <= $2 
		AND v.day >= $3 
		AND v.machine = us.event_filter 
		AND (us.last_sent_epoch < ($2 - 750) OR us.last_sent_epoch IS NULL)
		AND sy.disk_node_bytes_free::decimal / sy.disk_node_bytes_total < event_threshold
		AND v.created_trunc > NOW() - INTERVAL '1 hours' 
		group by us.user_id, machine
	`)
}

func collectMonitoringMachineCPULoad(notificationsByUserID map[uint64]map[types.EventName][]types.Notification) error {
	return collectMonitoringMachine(notificationsByUserID, types.MonitoringMachineCpuLoadEventName,
		`SELECT 
			max(us.id) as id, 
			us.user_id,
			machine 
		FROM users_subscriptions us 
		INNER JOIN (
			SELECT max(id) as id, user_id, machine, max(created_trunc) as created_trunc from stats_meta_p
			where process = 'system' AND day >= $3 
			group by user_id, machine
		) v ON us.user_id = v.user_id 
		WHERE v.machine = us.event_filter 
		AND us.event_name = $1 AND us.created_epoch <= $2 
		AND (us.last_sent_epoch < ($2 - 10) OR us.last_sent_epoch IS NULL)
		AND v.created_trunc > now() - interval '45 minutes' 
		AND event_threshold < (SELECT 
			1 - (cpu_node_idle_seconds_total::decimal - lag(cpu_node_idle_seconds_total::decimal, 4, 0::decimal) OVER (PARTITION BY m.user_id, machine ORDER BY sy.id asc)) / (cpu_node_system_seconds_total::decimal - lag(cpu_node_system_seconds_total::decimal, 4, 0::decimal) OVER (PARTITION BY m.user_id, machine ORDER BY sy.id asc)) as cpu_load 
			FROM stats_system as sy 
			INNER JOIN stats_meta_p m on meta_id = m.id 
			WHERE m.id = meta_id 
			AND m.day >= $3 
			AND m.user_id = v.user_id 
			AND m.machine = us.event_filter 
			ORDER BY sy.id desc
			LIMIT 1
		) 
		group by us.user_id, machine;
	`)
}

func collectMonitoringMachineMemoryUsage(notificationsByUserID map[uint64]map[types.EventName][]types.Notification) error {
	return collectMonitoringMachine(notificationsByUserID, types.MonitoringMachineMemoryUsageEventName,
		`SELECT 
			max(us.id) as id, 
			us.user_id,
			machine 
		FROM users_subscriptions us 
		INNER JOIN (
			SELECT max(id) as id, user_id, machine, max(created_trunc) as created_trunc from stats_meta_p
			where process = 'system' AND day >= $3 
			group by user_id, machine
		) v ON us.user_id = v.user_id 
		WHERE v.machine = us.event_filter 
		AND us.event_name = $1 AND us.created_epoch <= $2
		AND (us.last_sent_epoch < ($2 - 10) OR us.last_sent_epoch IS NULL)
		AND v.created_trunc > now() - interval '1 hours' 
		AND event_threshold < (SELECT avg(usage) FROM (SELECT 
		1 - ((memory_node_bytes_free + memory_node_bytes_cached + memory_node_bytes_buffers) / memory_node_bytes_total::decimal) as usage
		FROM stats_system as sy 
		INNER JOIN stats_meta_p m on meta_id = m.id 
		WHERE m.id = meta_id 
		AND m.day >= $3 
		AND m.user_id = v.user_id 
		AND m.machine = us.event_filter 
		ORDER BY sy.id desc
		LIMIT 5
		) p) 
		group by us.user_id, machine;
	`)
}

func collectMonitoringMachine(notificationsByUserID map[uint64]map[types.EventName][]types.Notification, eventName types.EventName, query string) error {
	latestEpoch := LatestEpoch()
	if latestEpoch == 0 {
		return nil
	}

	var dbResult []struct {
		SubscriptionID uint64 `db:"id"`
		UserID         uint64 `db:"user_id"`
		MachineName    string `db:"machine"`
	}

	now := time.Now()
	nowTs := now.Unix()
	var day int = int(nowTs/86400) - 1 // -1 so we have no issue on partition table change

	err := db.FrontendDB.Select(&dbResult, query, eventName, latestEpoch, day)
	if err != nil {
		return err
	}

	for _, r := range dbResult {

		n := &monitorMachineNotification{
			SubscriptionID: r.SubscriptionID,
			MachineName:    r.MachineName,
			UserID:         r.UserID,
			EventName:      eventName,
			Epoch:          latestEpoch,
		}

		if _, exists := notificationsByUserID[r.UserID]; !exists {
			notificationsByUserID[r.UserID] = map[types.EventName][]types.Notification{}
		}
		if _, exists := notificationsByUserID[r.UserID][n.GetEventName()]; !exists {
			notificationsByUserID[r.UserID][n.GetEventName()] = []types.Notification{}
		}
		notificationsByUserID[r.UserID][n.GetEventName()] = append(notificationsByUserID[r.UserID][n.GetEventName()], n)
	}

	return nil
}

type monitorMachineNotification struct {
	SubscriptionID uint64
	MachineName    string
	UserID         uint64
	Epoch          uint64
	EventName      types.EventName
}

func (n *monitorMachineNotification) GetEmailAttachment() *types.EmailAttachment {
	return nil
}

func (n *monitorMachineNotification) GetSubscriptionID() uint64 {
	return n.SubscriptionID
}

func (n *monitorMachineNotification) GetEpoch() uint64 {
	return n.Epoch
}

func (n *monitorMachineNotification) GetEventName() types.EventName {
	return n.EventName
}

func (n *monitorMachineNotification) GetInfo(includeUrl bool) string {
	switch n.EventName {
	case types.MonitoringMachineDiskAlmostFullEventName:
		return fmt.Sprintf(`Your staking machine "%v" is running low on storage space.`, n.MachineName)
	case types.MonitoringMachineOfflineEventName:
		return fmt.Sprintf(`Your staking machine "%v" might be offline. It has not been seen for a couple minutes now.`, n.MachineName)
	case types.MonitoringMachineCpuLoadEventName:
		return fmt.Sprintf(`Your staking machine "%v" has reached your configured CPU usage threshold.`, n.MachineName)
	case types.MonitoringMachineSwitchedToETH1FallbackEventName:
		return fmt.Sprintf(`Your staking machine "%v" has switched to your configured ETH1 fallback`, n.MachineName)
	case types.MonitoringMachineSwitchedToETH2FallbackEventName:
		return fmt.Sprintf(`Your staking machine "%v" has switched to your configured ETH2 fallback`, n.MachineName)
	case types.MonitoringMachineMemoryUsageEventName:
		return fmt.Sprintf(`Your staking machine "%v" has reached your configured RAM threshold.`, n.MachineName)
	}
	return ""
}

func (n *monitorMachineNotification) GetTitle() string {
	switch n.EventName {
	case types.MonitoringMachineDiskAlmostFullEventName:
		return "Storage Warning"
	case types.MonitoringMachineOfflineEventName:
		return "Staking Machine Offline"
	case types.MonitoringMachineCpuLoadEventName:
		return "High CPU Load"
	case types.MonitoringMachineSwitchedToETH1FallbackEventName:
		return "ETH1 Fallback Active"
	case types.MonitoringMachineSwitchedToETH2FallbackEventName:
		return "ETH2 Fallback Active"
	case types.MonitoringMachineMemoryUsageEventName:
		return "Memory Warning"
	}
	return ""
}

func (n *monitorMachineNotification) GetEventFilter() string {
	return n.MachineName
}

type taxReportNotification struct {
	SubscriptionID uint64
	UserID         uint64
	Epoch          uint64
	EventFilter    string
}

func (n *taxReportNotification) GetEmailAttachment() *types.EmailAttachment {
	tNow := time.Now()
	lastDay := time.Date(tNow.Year(), tNow.Month(), 1, 0, 0, 0, 0, time.UTC)
	firstDay := lastDay.AddDate(0, -1, 0)

	q, err := url.ParseQuery(n.EventFilter)

	if err != nil {
		logger.Warn("Failed to parse rewards report eventfilter")
		return nil
	}

	currency := q.Get("currency")

	validators := []uint64{}
	valSlice := strings.Split(q.Get("validators"), ",")
	if len(valSlice) > 0 {
		for _, val := range valSlice {
			v, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				continue
			}
			validators = append(validators, v)
		}
	} else {
		logger.Warn("Validators Not found in rewards report eventfilter")
		return nil
	}

	pdf := GetPdfReport(validators, currency, uint64(firstDay.Unix()), uint64(lastDay.Unix()))

	return &types.EmailAttachment{Attachment: pdf, Name: fmt.Sprintf("income_history_%v_%v.pdf", firstDay.Format("20060102"), lastDay.Format("20060102"))}
}

func (n *taxReportNotification) GetSubscriptionID() uint64 {
	return n.SubscriptionID
}

func (n *taxReportNotification) GetEpoch() uint64 {
	return n.Epoch
}

func (n *taxReportNotification) GetEventName() types.EventName {
	return types.TaxReportEventName
}

func (n *taxReportNotification) GetInfo(includeUrl bool) string {
	generalPart := fmt.Sprint(`Please find attached the income history of your selected validators.`)
	return generalPart
}

func (n *taxReportNotification) GetTitle() string {
	return fmt.Sprint("New report ready")
}

func (n *taxReportNotification) GetEventFilter() string {
	return n.EventFilter
}

func collectTaxReportNotificationNotifications(notificationsByUserID map[uint64]map[types.EventName][]types.Notification, eventName types.EventName) error {
	tNow := time.Now()
	firstDayOfMonth := time.Date(tNow.Year(), tNow.Month(), 1, 0, 0, 0, 0, time.UTC)
	if tNow.Year() == firstDayOfMonth.Year() && tNow.Month() == firstDayOfMonth.Month() && tNow.Day() == firstDayOfMonth.Day() { // Send the reports on the first day of the month
		var dbResult []struct {
			SubscriptionID uint64 `db:"id"`
			UserID         uint64 `db:"user_id"`
			Epoch          uint64 `db:"created_epoch"`
			EventFilter    string `db:"event_filter"`
		}

		err := db.DB.Select(&dbResult, `
			SELECT us.id, us.user_id, us.created_epoch, us.event_filter                 
			FROM users_subscriptions AS us
			WHERE us.event_name=$1 AND (us.last_sent_ts <= NOW() - INTERVAL '2 DAY' OR us.last_sent_ts IS NULL);
			`,
			eventName)

		if err != nil {
			return err
		}

		for _, r := range dbResult {
			n := &taxReportNotification{
				SubscriptionID: r.SubscriptionID,
				UserID:         r.UserID,
				Epoch:          r.Epoch,
				EventFilter:    r.EventFilter,
			}
			if _, exists := notificationsByUserID[r.UserID]; !exists {
				notificationsByUserID[r.UserID] = map[types.EventName][]types.Notification{}
			}
			if _, exists := notificationsByUserID[r.UserID][n.GetEventName()]; !exists {
				notificationsByUserID[r.UserID][n.GetEventName()] = []types.Notification{}
			}
			notificationsByUserID[r.UserID][n.GetEventName()] = append(notificationsByUserID[r.UserID][n.GetEventName()], n)
		}
	}

	return nil
}
