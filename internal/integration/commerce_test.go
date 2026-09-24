package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/xui-commerce/backend/internal/commerce"
	"example.com/xui-commerce/backend/internal/config"
	"example.com/xui-commerce/backend/internal/store"
	"example.com/xui-commerce/backend/internal/worker"
	"example.com/xui-commerce/backend/internal/xui"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set; isolated PostgreSQL integration tests were not run")
	}
	ctx := context.Background()
	adminCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := fmt.Sprintf("it_%x", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	appCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	if appCfg.ConnConfig.RuntimeParams == nil {
		appCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	appCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, appCfg)
	if err != nil {
		t.Fatal(err)
	}
	repo := &store.Store{DB: pool}
	if err = repo.Migrate(ctx); err != nil {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, dropErr := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		if dropErr != nil {
			t.Errorf("drop isolated schema: %v", dropErr)
		}
		admin.Close()
	})
	return repo, pool
}

func addPlan(t *testing.T, s *store.Store, deployment, kind, name string, price int64, maxPerDay int) int64 {
	t.Helper()
	var id int64
	err := s.DB.QueryRow(context.Background(), `INSERT INTO plans(deployment_id,panel_id,kind,name,enabled,is_limited,base_price_toman,price_per_extra_ip_toman,price_per_gb_toman,price_per_extra_month_toman,base_ip_limit,max_ip_limit,min_data_gb,max_data_bytes,expire_seconds,test_ip_limit,max_per_day,inbound_ids)
		VALUES($1,$2,$3,$4,true,false,$5,100,0,0,1,3,0,1073741824,3600,1,$6,'{1,2}') RETURNING id`, deployment, defaultPanel(deployment), kind, name, price, maxPerDay).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func defaultPanel(deployment string) string {
	switch deployment {
	case "retail-finland":
		return "panel-retail-finland"
	case "retail-germany":
		return "panel-retail-germany"
	default:
		return "panel-reseller-turk1"
	}
}
func resolve(t *testing.T, s *store.Store, deployment string, tg int64) *store.Actor {
	t.Helper()
	a, err := s.ResolveActor(context.Background(), deployment, tg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestMigrationsRerunAndTenantIdentityIsolation(t *testing.T) {
	s, _ := testStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var migrations int
	if err := s.DB.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations`).Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != 10 {
		t.Fatalf("migration rerun produced %d versions", migrations)
	}
	finland := resolve(t, s, "retail-finland", 81001)
	germany := resolve(t, s, "retail-germany", 81001)
	if finland.AccountID == germany.AccountID || finland.ID == germany.ID {
		t.Fatal("same Telegram ID across retail deployments was merged")
	}
	planID := addPlan(t, s, "retail-finland", "paid", "Finland only", 10000, 0)
	addPlan(t, s, "retail-germany", "paid", "Germany only", 20000, 0)
	plans, err := s.ListPlans(context.Background(), finland, "paid")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].ID != planID {
		t.Fatalf("deployment plan scope failed: %+v", plans)
	}
	q, err := s.CreateQuote(context.Background(), finland, planID, 1, 1, 0, "isolation-quote")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreatePurchase(context.Background(), germany, q.ID, "wallet", "foreign-checkout", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign actor accessed quote: %v", err)
	}
	privatePlan := addPlan(t, s, "retail-finland", "paid", "restricted", 5000, 0)
	if _, err = s.DB.Exec(context.Background(), `UPDATE plans SET is_global=false WHERE id=$1`, privatePlan); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(context.Background(), `INSERT INTO plan_access(deployment_id,plan_id,account_id) VALUES('retail-finland',$1,$2)`, privatePlan, finland.AccountID); err != nil {
		t.Fatal(err)
	}
	otherRetail := resolve(t, s, "retail-finland", 81002)
	visibleToOwner, err := s.ListPlans(context.Background(), finland, "paid")
	if err != nil {
		t.Fatal(err)
	}
	visibleToOther, err := s.ListPlans(context.Background(), otherRetail, "paid")
	if err != nil {
		t.Fatal(err)
	}
	if len(visibleToOwner) != 2 || len(visibleToOther) != 1 {
		t.Fatalf("account-specific plan access was not enforced: owner=%d other=%d", len(visibleToOwner), len(visibleToOther))
	}
	if _, err = s.CreateQuote(context.Background(), otherRetail, privatePlan, 1, 1, 0, "private-denied"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("restricted plan ID bypassed list visibility: %v", err)
	}
}

func TestRetailLifetimeTrialAndResellerDailyApprovalQuota(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(ctx, `UPDATE deployments SET retail_trial_reset_days=0,unapproved_trial_daily_limit=1 WHERE id='retail-finland'`); err != nil {
		t.Fatal(err)
	}
	retailPlan := addPlan(t, s, "retail-finland", "test", "retail-trial", 0, 3)
	retail := resolve(t, s, "retail-finland", 82001)
	one, err := s.ClaimTrial(ctx, retail, retailPlan, "retail-first")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ClaimTrial(ctx, retail, retailPlan, "retail-first")
	if err != nil {
		t.Fatal(err)
	}
	if one.SubscriptionID != replay.SubscriptionID {
		t.Fatal("same trial idempotency key created more than one subscription")
	}
	if _, err = s.ClaimTrial(ctx, retail, retailPlan, "retail-second"); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("retail lifetime policy allowed second claim: %v", err)
	}
	resellerPlan := addPlan(t, s, "reseller-turk1", "test", "reseller-trial", 0, 2)
	reseller := resolve(t, s, "reseller-turk1", 82002)
	if _, err = s.ClaimTrial(ctx, reseller, resellerPlan, "reseller-first"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimTrial(ctx, reseller, resellerPlan, "reseller-unapproved-second"); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("unapproved daily cap was not enforced: %v", err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE actors SET approval_status='approved' WHERE id=$1`, reseller.ID); err != nil {
		t.Fatal(err)
	}
	reseller, err = s.Actor(ctx, "reseller-turk1", reseller.TelegramID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimTrial(ctx, reseller, resellerPlan, "reseller-approved-second"); err != nil {
		t.Fatalf("approved plan daily cap should allow its second claim: %v", err)
	}
	if _, err = s.ClaimTrial(ctx, reseller, resellerPlan, "reseller-approved-third"); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("approved plan cap was not enforced: %v", err)
	}
	var period time.Time
	if err = s.DB.QueryRow(ctx, `SELECT period_date FROM trial_usage WHERE deployment_id='reseller-turk1'`).Scan(&period); err != nil {
		t.Fatal(err)
	}
	if period.Format("2006-01-02") != time.Now().UTC().Format("2006-01-02") {
		t.Fatalf("reseller quota did not use current UTC date: %s", period)
	}
}

func TestWalletPurchaseConcurrentReplayIsOneEffect(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	a := resolve(t, s, "retail-finland", 83001)
	planID := addPlan(t, s, "retail-finland", "paid", "wallet-plan", 10000, 0)
	if _, err := s.DB.Exec(ctx, `UPDATE commercial_accounts SET wallet_balance_toman=50000 WHERE id=$1`, a.AccountID); err != nil {
		t.Fatal(err)
	}
	q, err := s.CreateQuote(ctx, a, planID, 1, 1, 0, "wallet-quote")
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	results := make([]*store.PurchaseResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.CreatePurchase(ctx, a, q.ID, "wallet", "wallet-operation", "desk")
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			t.Fatalf("same concurrent request failed: %v", e)
		}
	}
	for _, v := range results {
		if v.OrderID != results[0].OrderID || v.SubscriptionID != results[0].SubscriptionID {
			t.Fatal("concurrent duplicate returned different purchase")
		}
	}
	bal, err := s.Balance(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 40000 {
		t.Fatalf("wallet debited more than once: %d", bal)
	}
	var ledger, orders, works int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger WHERE account_id=$1`, a.AccountID).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM orders WHERE account_id=$1`, a.AccountID).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM work_items WHERE account_id=$1`, a.AccountID).Scan(&works); err != nil {
		t.Fatal(err)
	}
	if ledger != 1 || orders != 1 || works != 1 {
		t.Fatalf("duplicate callback effects: ledger=%d orders=%d work=%d", ledger, orders, works)
	}
	if _, err = s.CreatePurchase(ctx, a, q.ID, "wallet", "wallet-operation", "different-input"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("reused key with different input accepted: %v", err)
	}
}

func TestDirectPaymentApprovalIsAuditedAndIdempotent(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	customer := resolve(t, s, "retail-finland", 84001)
	planID := addPlan(t, s, "retail-finland", "paid", "direct-plan", 15000, 0)
	q, err := s.CreateQuote(ctx, customer, planID, 1, 1, 0, "direct-quote")
	if err != nil {
		t.Fatal(err)
	}
	purchase, err := s.CreatePurchase(ctx, customer, q.ID, "direct", "direct-operation", "home")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SubmitReceipt(ctx, customer, purchase.PaymentIntentID, "telegram-file-id"); err != nil {
		t.Fatal(err)
	}
	admin := resolve(t, s, "retail-finland", 84002)
	if _, err = s.DB.Exec(ctx, `UPDATE actors SET role='admin',approval_status='approved' WHERE id=$1`, admin.ID); err != nil {
		t.Fatal(err)
	}
	admin, err = s.Actor(ctx, "retail-finland", admin.TelegramID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ApprovePayment(ctx, admin, purchase.PaymentIntentID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ApprovePayment(ctx, admin, purchase.PaymentIntentID)
	if err != nil {
		t.Fatal(err)
	}
	if first.OrderID != second.OrderID || first.Status != "provisioning" {
		t.Fatalf("unexpected approval result: %+v / %+v", first, second)
	}
	var settlements, works int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM payment_settlements WHERE intent_id=$1`, purchase.PaymentIntentID).Scan(&settlements); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM work_items WHERE payment_intent_id=$1`, purchase.PaymentIntentID).Scan(&works); err != nil {
		t.Fatal(err)
	}
	if settlements != 1 || works != 1 {
		t.Fatalf("approval had duplicate effects: settlement=%d work=%d", settlements, works)
	}
}

func TestRefundUsesImmutablePaidCapAndManualLegacyReview(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	customer := resolve(t, s, "retail-finland", 84501)
	planID := addPlan(t, s, "retail-finland", "paid", "refund-plan", 12000, 0)
	if _, err := s.DB.Exec(ctx, `UPDATE commercial_accounts SET wallet_balance_toman=20000 WHERE id=$1`, customer.AccountID); err != nil {
		t.Fatal(err)
	}
	q, err := s.CreateQuote(ctx, customer, planID, 1, 1, 0, "refund-quote")
	if err != nil {
		t.Fatal(err)
	}
	purchase, err := s.CreatePurchase(ctx, customer, q.ID, "wallet", "refund-buy", "refund")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE orders SET status='completed' WHERE id=$1`, purchase.OrderID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE subscriptions SET status='cancelled' WHERE id=$1`, purchase.SubscriptionID); err != nil {
		t.Fatal(err)
	}
	request, err := s.RequestRefund(ctx, customer, purchase.SubscriptionID, "refund-request", "service canceled")
	if err != nil {
		t.Fatal(err)
	}
	if request.RefundableCapToman != 12000 || request.SuggestedAmountToman != 12000 {
		t.Fatalf("refund cap did not use immutable purchase quote: %+v", request)
	}
	admin := resolve(t, s, "retail-finland", 84502)
	if _, err = s.DB.Exec(ctx, `UPDATE actors SET role='admin',approval_status='approved' WHERE id=$1`, admin.ID); err != nil {
		t.Fatal(err)
	}
	admin, err = s.Actor(ctx, "retail-finland", admin.TelegramID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveRefund(ctx, admin, request.ID, 13000, "operator reviewed receipt", "refund-approval", false); err == nil {
		t.Fatal("refund exceeding immutable paid amount was approved")
	}
	bal, err := s.ApproveRefund(ctx, admin, request.ID, 8000, "operator reviewed service cancellation", "refund-approval", false)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ApproveRefund(ctx, admin, request.ID, 8000, "operator reviewed service cancellation", "refund-approval", false)
	if err != nil {
		t.Fatal(err)
	}
	if bal != again {
		t.Fatalf("refund approval replay changed balance: %d / %d", bal, again)
	}
	var ledgerCount int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger WHERE reference_type='refund_request' AND reference_id=$1`, request.ID).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 1 {
		t.Fatalf("refund credited %d times", ledgerCount)
	}

	var legacySub int64
	err = s.DB.QueryRow(ctx, `INSERT INTO subscriptions(deployment_id,account_id,actor_id,panel_id,source_kind,status,client_email,client_uuid,sub_id)
		VALUES($1,$2,$3,$4,'legacy','cancelled','old@example.invalid','old-uuid','old-sub') RETURNING id`, customer.DeploymentID, customer.AccountID, customer.ID, "panel-retail-finland").Scan(&legacySub)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.RequestRefund(ctx, customer, legacySub, "legacy-refund", "historical subscription")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.RefundableCapToman != 0 || legacy.SuggestedAmountToman != 0 {
		t.Fatalf("legacy refund was reconstructed from today's catalog: %+v", legacy)
	}
	if _, err = s.ApproveRefund(ctx, admin, legacy.ID, 1000, "manual archive review", "legacy-refund-approval", false); err == nil {
		t.Fatal("legacy refund bypassed manual override")
	}
	if _, err = s.ApproveRefund(ctx, admin, legacy.ID, 1000, "legacy terms unavailable; operator reviewed archive", "legacy-refund-approval", true); err != nil {
		t.Fatal(err)
	}
}

type fakePanel struct {
	remote                *xui.RemoteClient
	outcome               xui.Outcome
	createOnUnknown       bool
	addCalls, attachCalls int
	addErr                error
}

func (p *fakePanel) CheckWriteReadiness(context.Context) error { return nil }
func (p *fakePanel) GetClient(_ context.Context, email string) (*xui.RemoteClient, error) {
	if p.remote == nil || p.remote.Email != email {
		return nil, xui.ErrNotFound
	}
	copy := *p.remote
	return &copy, nil
}
func (p *fakePanel) Add(_ context.Context, c xui.ClientConfig, inbounds []int) xui.WriteResult {
	p.addCalls++
	if p.outcome == xui.Succeeded || p.outcome == xui.Unknown && p.createOnUnknown {
		p.remote = &xui.RemoteClient{UUID: c.ID, Email: c.Email, SubID: c.SubID, Enable: c.Enable, ExpiryTime: c.ExpiryTime, LimitIP: c.LimitIP, TotalGB: c.TotalGB, LimitHWID: c.LimitHWID, InboundIDs: []int{inbounds[0]}}
	}
	return xui.WriteResult{Outcome: p.outcome, Err: p.addErr}
}
func (p *fakePanel) Attach(_ context.Context, _ string, inbounds []int) error {
	p.attachCalls++
	if p.remote != nil {
		p.remote.InboundIDs = append(p.remote.InboundIDs, inbounds...)
	}
	return errors.New("simulated uncertain attach response")
}
func (p *fakePanel) Delete(context.Context, string) xui.WriteResult {
	return xui.WriteResult{Outcome: xui.Unknown, Err: errors.New("simulated unknown delete")}
}
func (p *fakePanel) SubscriptionLinks(context.Context, string) ([]string, error) {
	return []string{"vless://test-link"}, nil
}

func TestPartialInboundAndCrashAfterRemoteSuccessRecoverWithoutDuplicateAdd(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	a := resolve(t, s, "retail-finland", 85001)
	planID := addPlan(t, s, "retail-finland", "paid", "recovery-plan", 10000, 0)
	if _, err := s.DB.Exec(ctx, `UPDATE commercial_accounts SET wallet_balance_toman=20000 WHERE id=$1`, a.AccountID); err != nil {
		t.Fatal(err)
	}
	q, err := s.CreateQuote(ctx, a, planID, 1, 1, 0, "partial-quote")
	if err != nil {
		t.Fatal(err)
	}
	purchase, err := s.CreatePurchase(ctx, a, q.ID, "wallet", "partial-operation", "recover")
	if err != nil {
		t.Fatal(err)
	}
	panel := &fakePanel{outcome: xui.Unknown, createOnUnknown: true, addErr: errors.New("success=false after independent inbound writes")}
	runner := &worker.Runner{Store: s, Config: config.Config{PanelTokens: map[string]string{"panel-retail-finland": "test-token"}}, PanelFactory: func(context.Context, string, string) (worker.Panel, error) { return panel, nil }}
	if err = runner.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if panel.addCalls != 1 || panel.attachCalls != 1 {
		t.Fatalf("partial repair did not use readback: add=%d attach=%d", panel.addCalls, panel.attachCalls)
	}
	var subStatus string
	var links string
	if err = s.DB.QueryRow(ctx, `SELECT status,subscription_links::text FROM subscriptions WHERE id=$1`, purchase.SubscriptionID).Scan(&subStatus, &links); err != nil {
		t.Fatal(err)
	}
	if subStatus != "active" || !strings.Contains(links, "vless://test-link") {
		t.Fatalf("verified partial add was not completed: status=%s links=%s", subStatus, links)
	}

	// Simulate a process crash after a successful panel write and before the DB completion commit.
	q2, err := s.CreateQuote(ctx, a, planID, 1, 1, 0, "restart-quote")
	if err != nil {
		t.Fatal(err)
	}
	restartPurchase, err := s.CreatePurchase(ctx, a, q2.ID, "wallet", "restart-operation", "crash")
	if err != nil {
		t.Fatal(err)
	}
	var workID int64
	var email, uuid, subid string
	var expiry int64
	var ip int
	var limit int64
	var tg int64
	var flow, comment string
	var inboundIDs []int32
	err = s.DB.QueryRow(ctx, `SELECT w.id,s.client_email,s.client_uuid,s.sub_id,s.expiry_time_ms,s.ip_limit,s.traffic_limit_bytes,a.telegram_id,s.flow,p.name,s.inbound_ids
		FROM work_items w JOIN subscriptions s ON s.id=w.subscription_id JOIN actors a ON a.id=w.actor_id JOIN plans p ON p.id=s.plan_id WHERE s.id=$1`, restartPurchase.SubscriptionID).Scan(&workID, &email, &uuid, &subid, &expiry, &ip, &limit, &tg, &flow, &comment, &inboundIDs)
	if err != nil {
		t.Fatal(err)
	}
	panel.remote = &xui.RemoteClient{UUID: uuid, Email: email, SubID: subid, Enable: true, ExpiryTime: expiry, LimitIP: ip, TotalGB: limit, Flow: flow, InboundIDs: []int{1, 2}}
	if _, err = s.DB.Exec(ctx, `UPDATE work_items SET status='running',phase='create_attempted',lease_until=now()-interval '1 second' WHERE id=$1`, workID); err != nil {
		t.Fatal(err)
	}
	if err = runner.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if panel.addCalls != 1 {
		t.Fatalf("recovery repeated non-idempotent add: %d calls", panel.addCalls)
	}
	if err = s.DB.QueryRow(ctx, `SELECT status FROM subscriptions WHERE id=$1`, restartPurchase.SubscriptionID).Scan(&subStatus); err != nil {
		t.Fatal(err)
	}
	if subStatus != "active" {
		t.Fatalf("restart did not adopt verified remote success: %s", subStatus)
	}
}

func TestUnknownAddPersistsManualReviewAndNeverBlindlyRetries(t *testing.T) {
	s, admin := testStore(t)
	ctx := context.Background()
	a := resolve(t, s, "retail-finland", 86001)
	planID := addPlan(t, s, "retail-finland", "paid", "unknown-plan", 5000, 0)
	if _, err := s.DB.Exec(ctx, `UPDATE commercial_accounts SET wallet_balance_toman=10000 WHERE id=$1`, a.AccountID); err != nil {
		t.Fatal(err)
	}
	q, err := s.CreateQuote(ctx, a, planID, 1, 1, 0, "unknown-quote")
	if err != nil {
		t.Fatal(err)
	}
	purchase, err := s.CreatePurchase(ctx, a, q.ID, "wallet", "unknown-operation", "uncertain")
	if err != nil {
		t.Fatal(err)
	}
	panel := &fakePanel{outcome: xui.Unknown, addErr: errors.New("connection reset after POST")}
	runner := &worker.Runner{Store: s, Config: config.Config{PanelTokens: map[string]string{"panel-retail-finland": "test-token"}}, PanelFactory: func(context.Context, string, string) (worker.Panel, error) { return panel, nil }}
	if err = runner.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if panel.addCalls != 1 {
		t.Fatalf("first add call count=%d", panel.addCalls)
	}
	if _, err = admin.Exec(ctx, `UPDATE work_items SET status='running',phase='create_attempted',attempts=4,lease_until=now()-interval '1 second' WHERE subscription_id=$1`, purchase.SubscriptionID); err != nil {
		t.Fatal(err)
	}
	if err = runner.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if panel.addCalls != 1 {
		t.Fatalf("unknown create was blindly retried: %d calls", panel.addCalls)
	}
	var status, phase string
	if err = s.DB.QueryRow(ctx, `SELECT status,phase FROM work_items WHERE subscription_id=$1`, purchase.SubscriptionID).Scan(&status, &phase); err != nil {
		t.Fatal(err)
	}
	if status != "manual_review" || phase != "manual_review" {
		t.Fatalf("unknown outcome was not retained for manual review: %s/%s", status, phase)
	}
}

var _ = commerce.GiB
