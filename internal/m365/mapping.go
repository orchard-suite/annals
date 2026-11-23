package m365

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/orchard-suite/platformkit/v3/annals"
)

// Map turns one audit record into the events it stands for. A record of a
// type or operation it does not know comes back with no events and ok
// false, so the caller can count what it skipped. Graph takes a record type
// in camel case in a query's filter and answers it in Pascal case on a
// record, so the type is matched without regard to case.
func Map(r Record) ([]annals.Event, bool) {
	switch strings.ToLower(r.RecordType) {
	case "sharepointsharingoperation":
		return mapSharing(r)
	case "copilotinteraction":
		return mapCopilot(r)
	}
	return nil, false
}

// sharing is the part of a SharePoint or OneDrive sharing record
// Annals keeps.
type sharing struct {
	Workload               string `json:"Workload"`
	ItemType               string `json:"ItemType"`
	ObjectID               string `json:"ObjectId"`
	SiteURL                string `json:"SiteUrl"`
	SourceFileName         string `json:"SourceFileName"`
	ListItemUniqueID       string `json:"ListItemUniqueId"`
	TargetUserOrGroupName  string `json:"TargetUserOrGroupName"`
	TargetUserOrGroupType  string `json:"TargetUserOrGroupType"`
	EventData              string `json:"EventData"`
	SharingLinkScope       string `json:"SharingLinkScope"`
	UniqueSharingID        string `json:"UniqueSharingId"`
	SiteSensitivityLabelID string `json:"SiteSensitivityLabelId"`
}

// shareOps grant someone access to the item; the rest of the sharing
// record type (links used, inheritance broken, access revoked) are kept
// under their own verb.
var shareOps = map[string]bool{
	"SharingInvitationCreated": true, "SharingSet": true, "AddedToSecureLink": true, "AddedToSharingLink": true,
	"AnonymousLinkCreated": true, "CompanyLinkCreated": true, "SecureLinkCreated": true, "SharingLinkCreated": true,
}

func mapSharing(r Record) ([]annals.Event, bool) {
	var s sharing
	if err := json.Unmarshal(r.AuditData, &s); err != nil {
		return nil, false
	}
	kind := map[string]string{"Web": "site", "Site": "site", "File": "file", "Folder": "folder", "List": "list"}[s.ItemType]
	if kind == "" {
		kind = "item"
	}
	system := "sharepoint"
	if strings.EqualFold(s.Workload, "OneDrive") {
		system = "onedrive"
	}
	id := s.ListItemUniqueID
	if id == "" {
		id = s.ObjectID
	}
	title := s.SourceFileName
	if kind == "site" || title == "" {
		title = s.SiteURL
	}
	verb := kind + ".sharing." + strings.ToLower(r.Operation)
	if shareOps[r.Operation] {
		verb = kind + ".share"
	}
	ev := annals.Event{
		At:       r.CreatedDateTime,
		EventKey: "ual:" + r.ID,
		Actor:    actor(r),
		Action: annals.Action{
			Verb:    slug(verb),
			Target:  annals.Target{System: system, Kind: kind, ID: shortID(id), Title: annals.Clip(title)},
			Outcome: "ok",
		},
		Labels: labels(r),
	}
	if p, ok := recipient(r.Operation, s); ok {
		ev.Parties = []annals.Party{p}
	}
	if g := grant(s.EventData); g != "" {
		ev.Change = &annals.Change{Summary: annals.Clip(g)}
	}
	return []annals.Event{ev}, true
}

// recipient is who an item was shared with: a person (a guest is external),
// a group, or anyone holding a link.
func recipient(op string, s sharing) (annals.Party, bool) {
	switch {
	case op == "AnonymousLinkCreated":
		return annals.Party{Role: "recipient", Kind: "anyone"}, true
	case op == "CompanyLinkCreated":
		return annals.Party{Role: "recipient", Kind: "group", ID: "organization", Name: "Everyone in the organization"}, true
	case s.TargetUserOrGroupName == "":
		return annals.Party{}, false
	}
	name := strings.TrimSpace(s.TargetUserOrGroupName)
	switch s.TargetUserOrGroupType {
	case "Guest", "Member", "User":
		p := annals.Party{Role: "recipient", Kind: annals.PersonKind, ID: shortID(strings.ToLower(name)), External: s.TargetUserOrGroupType == "Guest"}
		if emailRe.MatchString(name) {
			p.Email = strings.ToLower(name)
		} else {
			p.Name = short(name)
		}
		return p, true
	}
	return annals.Party{Role: "recipient", Kind: "group", ID: shortID(name), Name: short(name)}, true
}

// grant reads the permission a sharing record gave out of its event data,
// such as <PermissionsGranted>Edit</PermissionsGranted>.
var grantRe = regexp.MustCompile(`<(?:PermissionsGranted|Permission)>([^<]+)<`)

func grant(eventData string) string {
	if m := grantRe.FindStringSubmatch(eventData); m != nil {
		return strings.ToLower(m[1]) + " access"
	}
	return ""
}

// copilot is the part of a Copilot interaction record Annals keeps.
type copilot struct {
	CopilotEventData struct {
		AppHost           string `json:"AppHost"`
		ThreadID          string `json:"ThreadId"`
		AccessedResources []struct {
			ID                 string `json:"Id"`
			Name               string `json:"Name"`
			Type               string `json:"Type"`
			SiteURL            string `json:"SiteUrl"`
			Action             string `json:"Action"`
			SensitivityLabelID string `json:"SensitivityLabelId"`
		} `json:"AccessedResources"`
		AISystemPlugin []struct {
			ID   string `json:"Id"`
			Name string `json:"Name"`
		} `json:"AISystemPlugin"`
	} `json:"CopilotEventData"`
}

// mapCopilot makes one prompt record for the person, with every resource
// Copilot read for it, and one tool call per plugin or agent it set off,
// each caused by the prompt. The prompt's text is not in the audit log and
// never in Annals.
func mapCopilot(r Record) ([]annals.Event, bool) {
	var c copilot
	if err := json.Unmarshal(r.AuditData, &c); err != nil {
		return nil, false
	}
	d := c.CopilotEventData
	host := strings.ToLower(d.AppHost)
	if host == "" {
		host = "copilot"
	}
	person := actor(r)
	thread := d.ThreadID
	if thread == "" {
		thread = r.ID
	}
	key := "copilot:" + r.ID
	agent := &annals.Agent{Runtime: "copilot", Harness: short(slug("m365-" + host)), SessionID: shortID(thread)}
	prompt := annals.Event{
		At:       r.CreatedDateTime,
		EventKey: key,
		Actor:    person,
		Agent:    agent,
		Action: annals.Action{
			Verb:    "prompt.send",
			Target:  annals.Target{System: "m365-copilot", Kind: "thread", ID: shortID(thread), Title: annals.Clip(d.AppHost)},
			Outcome: "ok",
		},
		Labels: labels(r),
	}
	for _, res := range d.AccessedResources {
		if len(prompt.Items) == annals.MaxItems {
			break
		}
		id := res.ID
		if id == "" {
			id = res.SiteURL
		}
		if id == "" {
			continue
		}
		access := slug(strings.ToLower(res.Action))
		if access == "" {
			access = "read"
		}
		kind := slug(strings.ToLower(res.Type))
		if kind == "" {
			kind = "resource"
		}
		prompt.Items = append(prompt.Items, annals.Item{
			System: "m365", Kind: kind, ID: shortID(id), Title: annals.Clip(res.Name), Access: access, Label: short(res.SensitivityLabelID),
		})
	}
	out := []annals.Event{prompt}
	agentActor := annals.Actor{Kind: "agent", ID: short(slug("m365-" + host)), Name: "Microsoft 365 Copilot", OnBehalfOf: person.ID}
	if person.Kind != annals.PersonKind {
		agentActor.OnBehalfOf = ""
	}
	for n, p := range d.AISystemPlugin {
		id := p.ID
		if id == "" {
			id = p.Name
		}
		if id == "" {
			continue
		}
		out = append(out, annals.Event{
			At:       r.CreatedDateTime,
			EventKey: fmt.Sprintf("%s:tool-%d", key, n+1),
			Cause:    key,
			Actor:    agentActor,
			Agent:    agent,
			Action: annals.Action{
				Verb:    "tool.call",
				Target:  annals.Target{System: "m365-copilot", Kind: "tool", ID: shortID(id), Title: annals.Clip(p.Name)},
				Outcome: "ok",
			},
			Labels: labels(r),
		})
	}
	return out, true
}

// serviceTypes are the user types the audit log gives a service or an app.
var serviceTypes = map[string]bool{"Application": true, "ServicePrincipal": true, "System": true, "DcAdmin": true, "Reserved": true}

// serviceAccounts are Microsoft's own accounts that act with a regular
// user type, such as SharePoint's app account, which shares on a site's
// behalf.
var serviceAccounts = map[string]bool{"app@sharepoint": true, `sharepoint\system`: true}

// actor is who the record says acted: a person, or a system for the
// service and application accounts the audit log names. SharePoint can
// name the actor by a claims login, such as
// i:0h.f|membership|0000000000004242@live.example, whose last part need not be
// their email; the person is then posted by their user principal name,
// the id the directory knows them by.
func actor(r Record) annals.Actor {
	id := r.UserID
	if login, ok := claimsLogin(id); ok {
		id = login
		if upn := strings.ToLower(r.UserPrincipalName); emailRe.MatchString(upn) && !serviceAccounts[login] {
			id = upn
		}
	}
	if id == "" {
		id = r.UserPrincipalName
	}
	switch {
	case serviceTypes[r.UserType], serviceAccounts[strings.ToLower(id)]:
		return annals.Actor{Kind: "system", ID: shortID(id)}
	}
	a := annals.Actor{Kind: annals.PersonKind, ID: shortID(strings.ToLower(id))}
	if upn := strings.ToLower(r.UserPrincipalName); emailRe.MatchString(upn) && len(upn) <= annals.MaxShort {
		a.Email = upn
	}
	return a
}

// claimsLogin is the login a SharePoint claims id ends in, lowercased:
// i:0#.f|membership|user@example.com is user@example.com.
func claimsLogin(id string) (string, bool) {
	if !strings.HasPrefix(id, "i:") && !strings.HasPrefix(id, "c:") {
		return "", false
	}
	i := strings.LastIndexByte(id, '|')
	if i < 0 || i == len(id)-1 {
		return "", false
	}
	return strings.ToLower(id[i+1:]), true
}

func labels(r Record) map[string]string {
	l := map[string]string{"operation": short64(strings.ToLower(r.Operation))}
	if r.Service != "" {
		l["service"] = short64(strings.ToLower(r.Service))
	}
	return l
}

var emailRe = regexp.MustCompile(`[^\s@<>"']+@[^\s@<>"']+\.[A-Za-z]{2,}`)

var notSlug = regexp.MustCompile(`[^a-z0-9._-]+`)

func slug(s string) string {
	s = strings.Trim(notSlug.ReplaceAllString(strings.ToLower(s), "-"), "-._")
	return s
}

// short clips an id or name to the contract's limit on a character edge.
func short(s string) string {
	s = strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
	if len(s) <= annals.MaxShort {
		return s
	}
	cut := annals.MaxShort
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

// shortID keeps an id whole, or, past the limit, stands it in with its hash
// so two long ids never collide by clipping.
func shortID(s string) string {
	if len(s) <= annals.MaxShort && !strings.ContainsAny(s, "\n\r\x00") {
		return s
	}
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:16])
}

func short64(s string) string {
	if len(s) > annals.MaxLabelLen {
		return s[:annals.MaxLabelLen]
	}
	return s
}
