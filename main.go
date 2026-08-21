// Yaegi evaluates this at runtime via its exported factories, so there is no func main
// and it must stay out of `go build ./...`.
//go:build ignore

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/events"
	"code.vikunja.io/api/pkg/log"
	"code.vikunja.io/api/pkg/models"
	"code.vikunja.io/api/pkg/plugins"
	"code.vikunja.io/api/pkg/user"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/labstack/echo/v5"
)

// ApprisePlugin forwards Vikunja notifications to a self-hosted Apprise API
// instance (https://github.com/caronc/apprise-api), which fans them out to
// whatever services (Telegram, Pushover, ntfy, Discord, email, ...) the user
// has configured for their own Apprise config key. No push protocol, and no
// third-party credentials, live in this plugin or in Vikunja's database.
type ApprisePlugin struct{}

func (p *ApprisePlugin) Name() string    { return "apprise-notifications" }
func (p *ApprisePlugin) Version() string { return "0.2.0" }

func (p *ApprisePlugin) Init() error {
	events.RegisterListener((&models.TaskReminderFiredEvent{}).Name(), &ReminderListener{})
	events.RegisterListener((&models.TaskOverdueEvent{}).Name(), &OverdueListener{})
	events.RegisterListener((&models.TasksOverdueEvent{}).Name(), &OverdueDigestListener{})
	// pkg/notifications isn't exposed to yaegi, so the event is registered by its
	// well-known string name instead of notifications.NotificationCreatedEvent{}.Name().
	events.RegisterListener("notification.created", &GenericNotificationListener{})

	log.Infof("apprise-notifications plugin initialized")
	return nil
}

func (p *ApprisePlugin) Shutdown() error { return nil }

// RegisterAuthenticatedRoutes implements the AuthenticatedRouterPlugin interface
func (p *ApprisePlugin) RegisterAuthenticatedRoutes(g *echo.Group) {
	g.POST("/apprise/config", handleSetConfig)
	g.GET("/apprise/config", handleGetConfig)
	g.DELETE("/apprise/config", handleDeleteConfig)

	log.Infof("apprise-notifications plugin routes registered")
}

// --- Apprise API client -----------------------------------------------------
//
// Apprise API (https://github.com/caronc/apprise-api) has no built-in
// authentication on /add, /notify, etc. — by design on their side. It MUST run
// on an internal-only network, never reachable directly by end users. This
// plugin's authenticated routes are the only sanctioned entry point: every
// call below is scoped to the currently authenticated Vikunja user's own key.

// os.Getenv does not see the host process's real environment from inside a
// Yaegi-interpreted plugin (confirmed by live testing: it always returns "",
// even though the same env var is visibly set on the running process via ps).
// config.Key wraps viper directly — viper.AutomaticEnv() with SetEnvPrefix("vikunja")
// still binds e.g. VIKUNJA_PLUGINS_APPRISE_APIURL to this key without Vikunja core
// having to pre-declare it, and the actual env lookup happens in native code, not
// through the interpreter, so it isn't affected by whatever breaks os.Getenv here.
func appriseBaseURL() string {
	if v := config.Key("plugins.apprise.apiurl").GetString(); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:8000"
}

func appriseConfigKey(userID int64) string {
	prefix := config.Key("plugins.apprise.keyprefix").GetString()
	if prefix == "" {
		prefix = "vikunja-user-"
	}
	return prefix + strconv.FormatInt(userID, 10)
}

func sendApprise(userID int64, title, body string) error {
	payload, err := json.Marshal(map[string]string{"title": title, "body": body})
	if err != nil {
		return err
	}

	url := appriseBaseURL() + "/notify/" + appriseConfigKey(userID)
	resp, err := http.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil // no apprise config for this user yet — not an error
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("apprise api responded with %s", resp.Status)
	}
	return nil
}

// --- Config routes -----------------------------------------------------------

type configRequest struct {
	URLs []string `json:"urls"`
}

func handleSetConfig(c *echo.Context) error {
	s := db.NewSession()
	defer s.Close()

	u, err := user.GetCurrentUserFromDB(s, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "user not found")
	}

	req := &configRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	if len(req.URLs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "urls must contain at least one apprise:// url")
	}

	body, err := json.Marshal(map[string]string{"urls": strings.Join(req.URLs, ",")})
	if err != nil {
		return err
	}

	resp, err := http.Post(appriseBaseURL()+"/add/"+appriseConfigKey(u.ID), "application/json", bytes.NewReader(body))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "could not reach apprise api")
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return echo.NewHTTPError(http.StatusBadGateway, "apprise api rejected the config")
	}
	return c.NoContent(http.StatusNoContent)
}

func handleGetConfig(c *echo.Context) error {
	s := db.NewSession()
	defer s.Close()

	u, err := user.GetCurrentUserFromDB(s, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "user not found")
	}

	resp, err := http.Get(appriseBaseURL() + "/json/urls/" + appriseConfigKey(u.ID))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "could not reach apprise api")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return c.JSON(http.StatusOK, map[string]interface{}{"urls": []string{}})
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var parsed interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "apprise api returned an unexpected response")
	}
	return c.JSON(http.StatusOK, parsed)
}

func handleDeleteConfig(c *echo.Context) error {
	s := db.NewSession()
	defer s.Close()

	u, err := user.GetCurrentUserFromDB(s, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "user not found")
	}

	resp, err := http.Post(appriseBaseURL()+"/del/"+appriseConfigKey(u.ID), "application/json", nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "could not reach apprise api")
	}
	defer resp.Body.Close()

	return c.NoContent(http.StatusNoContent)
}

// --- task.reminder.fired -----------------------------------------------------
// Dispatched unconditionally (gated only by the instance-wide webhooks.enabled
// setting, not by the user's own email-reminder preference), unlike the DB-backed
// ReminderDueNotification, which task_reminder.go only persists when the user has
// email reminders enabled. Listening here means push reminders work independently
// of that email setting.

type reminderFiredPayload struct {
	Task struct {
		Title string `json:"title"`
	} `json:"task"`
	User struct {
		ID int64 `json:"id"`
	} `json:"user"`
	Project struct {
		Title string `json:"title"`
	} `json:"project"`
}

type ReminderListener struct{}

func (l *ReminderListener) Name() string { return "apprise.taskReminderFired" }

func (l *ReminderListener) Handle(msg *message.Message) error {
	p := &reminderFiredPayload{}
	if err := json.Unmarshal(msg.Payload, p); err != nil {
		return err
	}

	if err := sendApprise(p.User.ID, "Reminder: "+p.Task.Title, p.Project.Title); err != nil {
		log.Errorf("apprise-notifications: reminder push failed for user %d: %s", p.User.ID, err)
	}
	return nil
}

// --- task.overdue / tasks.overdue --------------------------------------------
// UndoneTaskOverdueNotification/UndoneTasksOverdueNotification have ToDB() return
// nil, so they never reach the notifications table or fire notification.created.
// These dedicated events are the only hook for overdue pushes.

type overdueSinglePayload struct {
	Task struct {
		Title string `json:"title"`
	} `json:"task"`
	User struct {
		ID int64 `json:"id"`
	} `json:"user"`
	Project struct {
		Title string `json:"title"`
	} `json:"project"`
}

type OverdueListener struct{}

func (l *OverdueListener) Name() string { return "apprise.taskOverdue" }

func (l *OverdueListener) Handle(msg *message.Message) error {
	p := &overdueSinglePayload{}
	if err := json.Unmarshal(msg.Payload, p); err != nil {
		return err
	}

	if err := sendApprise(p.User.ID, "Overdue: "+p.Task.Title, p.Project.Title); err != nil {
		log.Errorf("apprise-notifications: overdue push failed for user %d: %s", p.User.ID, err)
	}
	return nil
}

type overdueMultiPayload struct {
	Tasks []struct {
		Title string `json:"title"`
	} `json:"tasks"`
	User struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

type OverdueDigestListener struct{}

func (l *OverdueDigestListener) Name() string { return "apprise.tasksOverdue" }

func (l *OverdueDigestListener) Handle(msg *message.Message) error {
	p := &overdueMultiPayload{}
	if err := json.Unmarshal(msg.Payload, p); err != nil {
		return err
	}

	titles := make([]string, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		titles = append(titles, t.Title)
	}

	title := fmt.Sprintf("%d tasks overdue", len(titles))
	if err := sendApprise(p.User.ID, title, strings.Join(titles, ", ")); err != nil {
		log.Errorf("apprise-notifications: overdue digest push failed for user %d: %s", p.User.ID, err)
	}
	return nil
}

// --- notification.created (catch-all for everything else) -------------------

// dbNotification mirrors the columns of Vikunja's own notifications table that
// this plugin needs. Interpreted types reach xorm as anonymous reflect structs
// with no methods, so every query passes the table name explicitly via Table().
type dbNotification struct {
	ID           int64           `xorm:"pk autoincr"`
	NotifiableID int64           `xorm:"bigint not null"`
	Notification json.RawMessage `xorm:"json not null"`
	Name         string          `xorm:"varchar(250) not null"`
}

// genericNotificationPayload declares every optional field any notification's
// ToDB() JSON might contain; fields absent from a given payload just stay zero.
type genericNotificationPayload struct {
	Task struct {
		Title string `json:"title"`
	} `json:"task"`
	Project struct {
		Title string `json:"title"`
	} `json:"project"`
	Doer struct {
		Username string `json:"username"`
	} `json:"doer"`
	Assignee struct {
		Username string `json:"username"`
	} `json:"assignee"`
	Comment struct {
		Comment string `json:"comment"`
	} `json:"comment"`
	Member struct {
		Username string `json:"username"`
	} `json:"member"`
	Team struct {
		Name string `json:"name"`
	} `json:"team"`
}

func describeNotification(name string, raw json.RawMessage) (title, body string) {
	p := &genericNotificationPayload{}
	if err := json.Unmarshal(raw, p); err != nil {
		return "", ""
	}

	switch name {
	case "task.comment":
		return "New comment on " + p.Task.Title, p.Doer.Username + ": " + p.Comment.Comment
	case "task.assigned":
		return "Task assigned: " + p.Task.Title, "Assigned to " + p.Assignee.Username + " by " + p.Doer.Username
	case "task.deleted":
		return "Task deleted: " + p.Task.Title, "Deleted by " + p.Doer.Username
	case "project.created":
		return "New project: " + p.Project.Title, "Created by " + p.Doer.Username
	case "team.member.added":
		return "Added to team " + p.Team.Name, "By " + p.Doer.Username
	case "task.mentioned":
		return "Mentioned in " + p.Task.Title, "By " + p.Doer.Username
	default:
		return "Vikunja notification", name
	}
}

type GenericNotificationListener struct{}

func (l *GenericNotificationListener) Name() string { return "apprise.notificationCreated" }

func (l *GenericNotificationListener) Handle(msg *message.Message) error {
	created := &struct {
		NotificationID int64 `json:"notification_id"`
		UserID         int64 `json:"user_id"`
	}{}
	if err := json.Unmarshal(msg.Payload, created); err != nil {
		return err
	}

	s := db.NewSession()
	defer s.Close()

	row := &dbNotification{}
	has, err := s.Table("notifications").Where("id = ?", created.NotificationID).Get(row)
	if err != nil {
		return err
	}
	// task.reminder is already handled by ReminderListener via task.reminder.fired;
	// forwarding it again here would double-send.
	if !has || row.Name == "task.reminder" {
		return nil
	}

	title, body := describeNotification(row.Name, row.Notification)
	if title == "" {
		return nil
	}

	if err := sendApprise(created.UserID, title, body); err != nil {
		log.Errorf("apprise-notifications: push failed for user %d: %s", created.UserID, err)
	}
	return nil
}

var singleton = &ApprisePlugin{}

func NewPlugin() plugins.Plugin { return singleton }

// Typed factory function for Yaegi compatibility.
// Yaegi wraps return values per the declared return type, so a sub-interface type
// assertion (Plugin -> AuthenticatedRouterPlugin) doesn't work. This typed
// factory ensures Yaegi wraps the value with the correct interface wrapper.
func NewAuthenticatedRouterPlugin() plugins.AuthenticatedRouterPlugin { return singleton }
