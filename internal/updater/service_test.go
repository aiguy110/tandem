package updater

import (
	"context"
	"testing"

	"github.com/aiguy110/tandem/internal/notifications"
)

func TestServiceOffersUpdateThenRestart(t *testing.T) {
	center := notifications.New()
	restarted := false
	service := NewService(ServiceOptions{
		Center:  center,
		Updater: Options{CurrentVersion: "v1.0.0"},
		Check: func(context.Context, Options) (CheckResult, error) {
			return CheckResult{CurrentVersion: "v1.0.0", LatestVersion: "v1.1.0", Available: true}, nil
		},
		Update:      func(context.Context, Options) (bool, error) { return true, nil },
		NeedsReview: func() (bool, error) { return false, nil },
		Restart:     func() { restarted = true },
	})
	service.poll(context.Background())
	item := onlyNotification(t, center)
	if item.Title != "Tandem update available" || len(item.Actions) != 1 || item.Actions[0].ID != "install" {
		t.Fatalf("available notification = %#v", item)
	}
	if _, err := service.HandleAction(context.Background(), notificationID, "install"); err != nil {
		t.Fatal(err)
	}
	item = onlyNotification(t, center)
	if item.Title != "Tandem update complete" || item.Actions[0].ID != "restart" {
		t.Fatalf("complete notification = %#v", item)
	}
	if _, err := service.HandleAction(context.Background(), notificationID, "restart"); err != nil {
		t.Fatal(err)
	}
	if !restarted {
		t.Fatal("restart callback was not invoked")
	}
}

func TestServiceWaitsForConfigReview(t *testing.T) {
	center := notifications.New()
	needsReview := true
	service := NewService(ServiceOptions{
		Center:  center,
		Updater: Options{CurrentVersion: "v1.0.0"},
		Check: func(context.Context, Options) (CheckResult, error) {
			return CheckResult{CurrentVersion: "v1.0.0", LatestVersion: "v1.1.0", Available: true}, nil
		},
		Update:      func(context.Context, Options) (bool, error) { return true, nil },
		NeedsReview: func() (bool, error) { return needsReview, nil },
	})
	service.poll(context.Background())
	if _, err := service.HandleAction(context.Background(), notificationID, "install"); err != nil {
		t.Fatal(err)
	}
	item := onlyNotification(t, center)
	if item.Title != "Configuration review needed" || item.Actions[0].ID != "configure" {
		t.Fatalf("review notification = %#v", item)
	}
	needsReview = false
	service.poll(context.Background())
	if got := onlyNotification(t, center).Title; got != "Tandem update complete" {
		t.Fatalf("title after completed review = %q", got)
	}
}

func onlyNotification(t *testing.T, center *notifications.Center) notifications.Notification {
	t.Helper()
	items := center.List()
	if len(items) != 1 {
		t.Fatalf("notifications = %#v", items)
	}
	return items[0]
}
