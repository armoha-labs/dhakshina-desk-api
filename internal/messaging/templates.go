// Package messaging sends WhatsApp messages from the queue, tracks delivery
// from Meta's webhook, answers simple parent replies, and runs the daily
// scheduler (reminders, trial-ending notices, held-job release).
package messaging

// Platform-owned utility templates. Each must be created and approved in
// WhatsApp Manager with exactly these names, body variables and buttons
// before production sends work. Every body opens with the class name and
// signs off "via Dhakshina Desk".
//
//	dd_login_code         authentication: {{1}} code; copy-code button
//	dd_plan_link          utility: URL button WEB_BASE_URL/subscribe/{{1}}
//	dd_subscription_paid  utility: {{1}} plan name, {{2}} paid until
//	dd_trial_ending       utility: {{1}} class, {{2}} trial end date; URL button /subscribe/{{1}}
//	dd_fee_reminder       utility: {{1}} class, {{2}} student, {{3}} batch, {{4}} amount, {{5}} months; URL button /p/{{1}}
//	dd_fee_receipt        utility: {{1}} class, {{2}} amount, {{3}} student, {{4}} batch, {{5}} receipt no, {{6}} paid on; URL button /p/{{1}}
//	dd_fee_notice         utility: {{1}} class, {{2}} student, {{3}} batch; URL button /p/{{1}}
//	dd_consent_request    utility: {{1}} class, {{2}} guardian; URL button /p/{{1}} (suffix "<token>/consent"),
//	                      quick replies "I agree" and "No"
const (
	TplLoginCode      = "dd_login_code"
	TplPlanLink       = "dd_plan_link"
	TplPaid           = "dd_subscription_paid"
	TplTrialEnding    = "dd_trial_ending"
	TplFeeReminder    = "dd_fee_reminder"
	TplFeeReceipt     = "dd_fee_receipt"
	TplFeeNotice      = "dd_fee_notice"
	TplConsentRequest = "dd_consent_request"
)
