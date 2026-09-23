package ownership

import (
	"reflect"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/merge"
	"github.com/klrushka/llm-proxy/internal/policy"
)

func ent(t detection.Type, start, end int, conf float64, sources ...detection.Source) merge.Entity {
	return merge.Entity{
		Candidate: detection.Candidate{Type: t, Start: start, End: end, Confidence: conf, Sources: sources},
	}
}

func idx(t *testing.T, text, sub string) int {
	t.Helper()
	i := strings.Index(text, sub)
	if i < 0 {
		t.Fatalf("substring %q not found in %q", sub, text)
	}
	return i
}

func find(t *testing.T, got []Entity, typ detection.Type) *Entity {
	t.Helper()
	for i := range got {
		if got[i].Type == typ {
			return &got[i]
		}
	}
	t.Fatalf("no entity of type %s in %+v", typ, got)
	return nil
}

func TestClientPassportSharedOwner(t *testing.T) {
	text := "Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ, паспорт 00 00 000000"
	nameStart := idx(t, text, "ТЕСТОВ ТЕСТ ТЕСТОВИЧ")
	passStart := idx(t, text, "00 00 000000")
	in := []merge.Entity{
		ent(detection.TypePassportNumber, passStart, passStart+len("00 00 000000"), 1.0, detection.SourceRegex, detection.SourceValidator),
		ent(detection.TypeFullName, nameStart, nameStart+len("ТЕСТОВ ТЕСТ ТЕСТОВИЧ"), 0.95, detection.SourceRubert),
	}

	got := Assess(text, in)
	if len(got) != 2 {
		t.Fatalf("Assess() = %d results, want 2", len(got))
	}

	name := find(t, got, detection.TypeFullName)
	pass := find(t, got, detection.TypePassportNumber)
	if !name.Personal || !pass.Personal {
		t.Errorf("name.Personal=%v pass.Personal=%v, want both true", name.Personal, pass.Personal)
	}
	if name.OwnerType != OwnerTypePerson || pass.OwnerType != OwnerTypePerson {
		t.Errorf("owner types = %s/%s, want PERSON/PERSON", name.OwnerType, pass.OwnerType)
	}
	if name.OwnerID == "" || name.OwnerID != pass.OwnerID {
		t.Errorf("owner ids = %q/%q, want same non-empty", name.OwnerID, pass.OwnerID)
	}
	if name.ReviewRecommended || pass.ReviewRecommended {
		t.Errorf("review recommended on personal entities")
	}
	if !hasReason(name.ReasonCodes, ReasonClientContext) {
		t.Errorf("name reasons %v missing client_context", name.ReasonCodes)
	}
	if !hasReason(pass.ReasonCodes, ReasonPassportContext) {
		t.Errorf("passport reasons %v missing passport_context", pass.ReasonCodes)
	}
	if !hasReason(name.ReasonCodes, ReasonLinkedEntities) {
		t.Errorf("name reasons %v missing linked_entities", name.ReasonCodes)
	}
	if !hasReason(pass.ReasonCodes, ReasonLinkedEntities) {
		t.Errorf("passport reasons %v missing linked_entities", pass.ReasonCodes)
	}
}

func TestDataClientBlockSharesOwner(t *testing.T) {
	text := "Иванов Иван Иванович, телефон: +7 900 123-45-67, email: ivanov@example.com, дата рождения 01.02.1990"
	nameStart := idx(t, text, "Иванов Иван Иванович")
	phoneStart := idx(t, text, "+7 900 123-45-67")
	emailStart := idx(t, text, "ivanov@example.com")
	birthStart := idx(t, text, "01.02.1990")
	in := []merge.Entity{
		ent(detection.TypeFullName, nameStart, nameStart+len("Иванов Иван Иванович"), 0.95, detection.SourceRubert),
		ent(detection.TypePhone, phoneStart, phoneStart+len("+7 900 123-45-67"), 0.9, detection.SourceRegex),
		ent(detection.TypeEmail, emailStart, emailStart+len("ivanov@example.com"), 0.95, detection.SourceRegex),
		ent(detection.TypeBirthDate, birthStart, birthStart+len("01.02.1990"), 0.9, detection.SourceValidator),
	}

	got := Assess(text, in)
	if len(got) != 4 {
		t.Fatalf("Assess() = %d results, want 4", len(got))
	}
	owner := ""
	for i := range got {
		if !got[i].Personal || got[i].OwnerType != OwnerTypePerson {
			t.Errorf("entity %s not personal PERSON: %+v", got[i].Type, got[i])
		}
		if got[i].OwnerID == "" {
			t.Errorf("entity %s has empty owner id", got[i].Type)
		}
		if owner == "" {
			owner = got[i].OwnerID
		} else if got[i].OwnerID != owner {
			t.Errorf("entity %s owner %q != %q", got[i].Type, got[i].OwnerID, owner)
		}
	}
}

func TestPushkinPublic(t *testing.T) {
	text := "Александр Пушкин — русский поэт."
	nameStart := idx(t, text, "Александр Пушкин")
	in := []merge.Entity{
		ent(detection.TypeFullName, nameStart, nameStart+len("Александр Пушкин"), 0.95, detection.SourceRubert),
	}

	got := Assess(text, in)
	if len(got) != 1 {
		t.Fatalf("Assess() = %d results, want 1", len(got))
	}
	e := got[0]
	if e.Personal {
		t.Errorf("Pushkin personal = true, want false")
	}
	if e.OwnerType != OwnerTypePublic {
		t.Errorf("owner type = %s, want PUBLIC", e.OwnerType)
	}
	if e.ReviewRecommended {
		t.Errorf("review recommended = true, want false")
	}
	if !hasReason(e.ReasonCodes, ReasonPublicContext) {
		t.Errorf("reasons %v missing public_context", e.ReasonCodes)
	}
}

func TestBankBranchAddressOrganization(t *testing.T) {
	text := "Отделение банка находится по адресу: г. Москва, ул. Тестовая, д. 1"
	addrStart := idx(t, text, "г. Москва, ул. Тестовая, д. 1")
	in := []merge.Entity{
		ent(detection.TypeAddress, addrStart, addrStart+len("г. Москва, ул. Тестовая, д. 1"), 0.9, detection.SourceGliner),
	}

	got := Assess(text, in)
	if len(got) != 1 {
		t.Fatalf("Assess() = %d results, want 1", len(got))
	}
	e := got[0]
	if e.Personal {
		t.Errorf("address personal = true, want false")
	}
	if e.OwnerType != OwnerTypeOrganization {
		t.Errorf("owner type = %s, want ORGANIZATION", e.OwnerType)
	}
	if e.ReviewRecommended {
		t.Errorf("review recommended = true, want false")
	}
	if !hasReason(e.ReasonCodes, ReasonOrganizationContext) {
		t.Errorf("reasons %v missing organization_context", e.ReasonCodes)
	}
}

func TestOrganizationHardNegatives(t *testing.T) {
	cases := []struct {
		name string
		text string
		sub  string
		typ  detection.Type
	}{
		{"ooo", "ООО Ромашка, ИНН 7701234567", "Ромашка", detection.TypeFullName},
		{"ao", "АО ТехноПром, юридический адрес: г. Москва", "ТехноПром", detection.TypeFullName},
		{"pao", "ПАО Газпром, филиал в г. Москве", "Газпром", detection.TypeFullName},
		{"legal-address", "юридический адрес: г. Москва, ул. Ленина, д. 5", "г. Москва, ул. Ленина, д. 5", detection.TypeAddress},
		{"org-inn", "ИНН организации 7701234567", "7701234567", detection.TypeINNPerson},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := idx(t, tc.text, tc.sub)
			in := []merge.Entity{
				ent(tc.typ, start, start+len(tc.sub), 0.9, detection.SourceRubert),
			}
			got := Assess(tc.text, in)
			if len(got) != 1 {
				t.Fatalf("Assess() = %d results, want 1", len(got))
			}
			e := got[0]
			if e.Personal {
				t.Errorf("personal = true, want false")
			}
			if e.OwnerType != OwnerTypeOrganization {
				t.Errorf("owner type = %s, want ORGANIZATION", e.OwnerType)
			}
			if e.ReviewRecommended {
				t.Errorf("review recommended = true, want false")
			}
		})
	}
}

func TestUnknownAmbiguous(t *testing.T) {
	text := "Встреча состоится в городе Тестовск."
	locStart := idx(t, text, "Тестовск")
	in := []merge.Entity{
		ent(detection.TypeAddressCity, locStart, locStart+len("Тестовск"), 0.9, detection.SourceGliner),
	}

	got := Assess(text, in)
	if len(got) != 1 {
		t.Fatalf("Assess() = %d results, want 1", len(got))
	}
	e := got[0]
	if e.Personal {
		t.Errorf("personal = true, want false")
	}
	if e.OwnerType != OwnerTypeUnknown {
		t.Errorf("owner type = %s, want UNKNOWN", e.OwnerType)
	}
	if !e.ReviewRecommended {
		t.Errorf("review recommended = false, want true")
	}
	if e.OwnerID != "" {
		t.Errorf("owner id = %q, want empty", e.OwnerID)
	}
	if !hasReason(e.ReasonCodes, ReasonAmbiguous) {
		t.Errorf("reasons %v missing ambiguous_context", e.ReasonCodes)
	}
}

func TestNegativeOverridesPositiveEvidence(t *testing.T) {
	text := "Клиент Александр Пушкин — русский поэт."
	nameStart := idx(t, text, "Александр Пушкин")
	in := []merge.Entity{
		ent(detection.TypeFullName, nameStart, nameStart+len("Александр Пушкин"), 0.95, detection.SourceRubert),
	}

	got := Assess(text, in)
	if len(got) != 1 {
		t.Fatalf("Assess() = %d results, want 1", len(got))
	}
	e := got[0]
	if e.Personal {
		t.Errorf("personal = true, want false (public overrides client)")
	}
	if e.OwnerType != OwnerTypePublic {
		t.Errorf("owner type = %s, want PUBLIC", e.OwnerType)
	}
	if e.ReviewRecommended {
		t.Errorf("review recommended = true, want false")
	}
}

func TestPositiveMarkersGivePositiveContext(t *testing.T) {
	tests := []struct {
		text string
		sub  string
		typ  detection.Type
	}{
		{"дата рождения 01.02.1990", "01.02.1990", detection.TypeBirthDate},
		{"адрес регистрации: Москва", "Москва", detection.TypeAddress},
		{"место рождения город Тестовск", "Тестовск", detection.TypeBirthPlace},
	}
	for _, tt := range tests {
		start := idx(t, tt.text, tt.sub)
		in := []merge.Entity{ent(tt.typ, start, start+len(tt.sub), 0.9, detection.SourceRubert)}
		got := Assess(tt.text, in)
		if len(got) != 1 {
			t.Fatalf("Assess(%q) = %d results, want 1", tt.text, len(got))
		}
		e := got[0]
		if !e.Personal {
			t.Errorf("Assess(%q) personal = false, want true", tt.text)
		}
		if e.OwnerType != OwnerTypePerson {
			t.Errorf("Assess(%q) owner type = %s, want PERSON", tt.text, e.OwnerType)
		}
		if !hasReason(e.ReasonCodes, ReasonPositiveContext) {
			t.Errorf("Assess(%q) reasons = %v, want ReasonPositiveContext", tt.text, e.ReasonCodes)
		}
	}
}

// TestExplicitHighRiskTypesPersonalWithoutName proves that canonical high-risk
// types whose detection is already structural, validator-backed or marker-based
// become personal without any name co-occurrence or field label phrase. Each
// entity is classified PERSON with the stable field_label reason.
func TestExplicitHighRiskTypesPersonalWithoutName(t *testing.T) {
	tests := []struct {
		name string
		text string
		sub  string
		typ  detection.Type
	}{
		{"email", "ivanov@example.com", "ivanov@example.com", detection.TypeEmail},
		{"phone", "+7 900 123-45-67", "+7 900 123-45-67", detection.TypePhone},
		{"passport number", "паспорт 00 00 000000", "00 00 000000", detection.TypePassportNumber},
		{"passport division code", "код подразделения 000-000", "000-000", detection.TypePassportDivisionCode},
		{"passport issue date", "дата выдачи 03.04.2020", "03.04.2020", detection.TypePassportIssueDate},
		{"passport issuer", "кем выдан: ОВД района", "ОВД района", detection.TypePassportIssuer},
		{"driver license number", "водительское удостоверение 7777 123456", "7777 123456", detection.TypeDriverLicenseNumber},
		{"inn person", "ИНН физлица 123456789047", "123456789047", detection.TypeINNPerson},
		{"bank card number", "Карта 4111111111111111", "4111111111111111", detection.TypeBankCardNumber},
		{"card cvv", "CVV: 123", "123", detection.TypeCardCVV},
		{"card pin", "PIN: 4567", "4567", detection.TypeCardPIN},
		{"cardholder name", "Cardholder: Ivan Ivanov", "Ivan Ivanov", detection.TypeCardholderName},
		{"citizenship", "гражданство: Российская Федерация", "Российская Федерация", detection.TypeCitizenship},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := idx(t, tt.text, tt.sub)
			in := []merge.Entity{ent(tt.typ, start, start+len(tt.sub), 0.9, detection.SourceRegex)}
			got := Assess(tt.text, in)
			if len(got) != 1 {
				t.Fatalf("Assess(%q) = %d results, want 1", tt.text, len(got))
			}
			e := got[0]
			if !e.Personal {
				t.Errorf("Assess(%q) personal = false, want true", tt.text)
			}
			if e.OwnerType != OwnerTypePerson {
				t.Errorf("Assess(%q) owner type = %s, want PERSON", tt.text, e.OwnerType)
			}
			if e.ReviewRecommended {
				t.Errorf("Assess(%q) review recommended = true, want false", tt.text)
			}
			if !hasReason(e.ReasonCodes, ReasonFieldLabel) {
				t.Errorf("Assess(%q) reasons = %v, want ReasonFieldLabel", tt.text, e.ReasonCodes)
			}
		})
	}
}

// TestExplicitHighRiskTypeOrganizationPrecedence proves that explicit
// organization context still wins over the explicit high-risk path: a bank
// branch address and a legal-entity INN never become personal.
func TestExplicitHighRiskTypeOrganizationPrecedence(t *testing.T) {
	tests := []struct {
		name string
		text string
		sub  string
		typ  detection.Type
	}{
		{"bank branch address", "Отделение банка находится по адресу: г. Москва, ул. Тестовая, д. 1", "г. Москва, ул. Тестовая, д. 1", detection.TypeAddress},
		{"org inn", "ИНН организации 123456789047", "123456789047", detection.TypeINNPerson},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := idx(t, tt.text, tt.sub)
			in := []merge.Entity{ent(tt.typ, start, start+len(tt.sub), 0.9, detection.SourceRegex)}
			got := Assess(tt.text, in)
			if len(got) != 1 {
				t.Fatalf("Assess(%q) = %d results, want 1", tt.text, len(got))
			}
			e := got[0]
			if e.Personal {
				t.Errorf("Assess(%q) personal = true, want false (organization precedence)", tt.text)
			}
			if e.OwnerType != OwnerTypeOrganization {
				t.Errorf("Assess(%q) owner type = %s, want ORGANIZATION", tt.text, e.OwnerType)
			}
			if e.ReviewRecommended {
				t.Errorf("Assess(%q) review recommended = true, want false", tt.text)
			}
		})
	}
}

// TestExplicitHighRiskTypePublicPrecedence proves that explicit public/literary
// context still wins over the explicit high-risk path: a public person mention
// never becomes personal.
func TestExplicitHighRiskTypePublicPrecedence(t *testing.T) {
	text := "Александр Пушкин — русский поэт."
	nameStart := idx(t, text, "Александр Пушкин")
	in := []merge.Entity{
		ent(detection.TypeFullName, nameStart, nameStart+len("Александр Пушкин"), 0.95, detection.SourceRubert),
	}
	got := Assess(text, in)
	if len(got) != 1 {
		t.Fatalf("Assess() = %d results, want 1", len(got))
	}
	e := got[0]
	if e.Personal {
		t.Errorf("personal = true, want false (public precedence)")
	}
	if e.OwnerType != OwnerTypePublic {
		t.Errorf("owner type = %s, want PUBLIC", e.OwnerType)
	}
	if e.ReviewRecommended {
		t.Errorf("review recommended = true, want false")
	}
}

// TestExplicitHighRiskTypeWinsOverBankContext proves that explicit high-risk
// types stay personal even when a neighboring organization word such as "банк"
// or "отделение" appears in the local context. The email and phone in the
// example must remain personal and be masked.
func TestExplicitHighRiskTypeWinsOverBankContext(t *testing.T) {
	text := "Банк просит клиента указать email ivanov@example.com и телефон +7 900 123-45-67"
	emailStart := idx(t, text, "ivanov@example.com")
	phoneStart := idx(t, text, "+7 900 123-45-67")
	in := []merge.Entity{
		ent(detection.TypeEmail, emailStart, emailStart+len("ivanov@example.com"), 0.95, detection.SourceRegex),
		ent(detection.TypePhone, phoneStart, phoneStart+len("+7 900 123-45-67"), 0.9, detection.SourceRegex),
	}
	got := Assess(text, in)
	if len(got) != 2 {
		t.Fatalf("Assess() = %d results, want 2", len(got))
	}
	for _, e := range got {
		if !e.Personal {
			t.Errorf("entity %s personal = false, want true (bank context must not demote)", e.Type)
		}
		if e.OwnerType != OwnerTypePerson {
			t.Errorf("entity %s owner type = %s, want PERSON", e.Type, e.OwnerType)
		}
		if e.ReviewRecommended {
			t.Errorf("entity %s review recommended = true, want false", e.Type)
		}
		if !hasReason(e.ReasonCodes, ReasonFieldLabel) {
			t.Errorf("entity %s reasons = %v, want ReasonFieldLabel", e.Type, e.ReasonCodes)
		}
	}
}

// TestExplicitHighRiskTypeWinsOverOrgPublicContext proves that a sensitive
// credential and other explicit high-risk types stay personal even when an
// organization or public marker appears nearby.
func TestExplicitHighRiskTypeWinsOverOrgPublicContext(t *testing.T) {
	tests := []struct {
		name string
		text string
		sub  string
		typ  detection.Type
	}{
		{"card near bank", "Банк выпустил карту 4111111111111111", "4111111111111111", detection.TypeBankCardNumber},
		{"cvv near отделение", "Отделение банка, CVV: 123", "123", detection.TypeCardCVV},
		{"passport near филиал", "Филиал банка, паспорт 00 00 000000", "00 00 000000", detection.TypePassportNumber},
		{"citizenship near поэт", "Поэт указал гражданство: Российская Федерация", "Российская Федерация", detection.TypeCitizenship},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := idx(t, tt.text, tt.sub)
			in := []merge.Entity{ent(tt.typ, start, start+len(tt.sub), 0.9, detection.SourceRegex)}
			got := Assess(tt.text, in)
			if len(got) != 1 {
				t.Fatalf("Assess(%q) = %d results, want 1", tt.text, len(got))
			}
			e := got[0]
			if !e.Personal {
				t.Errorf("Assess(%q) personal = false, want true (org/public context must not demote)", tt.text)
			}
			if e.OwnerType != OwnerTypePerson {
				t.Errorf("Assess(%q) owner type = %s, want PERSON", tt.text, e.OwnerType)
			}
			if e.ReviewRecommended {
				t.Errorf("Assess(%q) review recommended = true, want false", tt.text)
			}
		})
	}
}

// TestINNOrganizationException proves that the narrow explicit INN-organization
// context keeps an INN_PERSON candidate ORGANIZATION even though INN_PERSON is
// an explicit high-risk type.
func TestINNOrganizationException(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"инн организации", "ИНН организации 123456789047"},
		{"инн юрлица", "ИНН юрлица 123456789047"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := idx(t, tt.text, "123456789047")
			in := []merge.Entity{ent(detection.TypeINNPerson, start, start+len("123456789047"), 0.9, detection.SourceRegex)}
			got := Assess(tt.text, in)
			if len(got) != 1 {
				t.Fatalf("Assess(%q) = %d results, want 1", tt.text, len(got))
			}
			e := got[0]
			if e.Personal {
				t.Errorf("Assess(%q) personal = true, want false (INN organization exception)", tt.text)
			}
			if e.OwnerType != OwnerTypeOrganization {
				t.Errorf("Assess(%q) owner type = %s, want ORGANIZATION", tt.text, e.OwnerType)
			}
			if e.ReviewRecommended {
				t.Errorf("Assess(%q) review recommended = true, want false", tt.text)
			}
		})
	}
}

// TestExplicitHighRiskTypeDisabledByPolicy proves that a disabled explicit
// high-risk type is not tokenized and does not affect the allowed ownership of
// a co-occurring allowed type.
func TestExplicitHighRiskTypeDisabledByPolicy(t *testing.T) {
	text := "Иванов Иван Иванович, паспорт 00 00 000000"
	nameStart := idx(t, text, "Иванов Иван Иванович")
	passStart := idx(t, text, "00 00 000000")
	in := []merge.Entity{
		ent(detection.TypeFullName, nameStart, nameStart+len("Иванов Иван Иванович"), 0.95, detection.SourceRubert),
		ent(detection.TypePassportNumber, passStart, passStart+len("00 00 000000"), 1.0, detection.SourceRegex),
	}
	assessed := Assess(text, in)
	p := policy.NewPolicy([]string{string(detection.TypeFullName)})

	got := ApplyPolicy(assessed, p)
	if len(got) != 2 {
		t.Fatalf("ApplyPolicy() = %d results, want 2", len(got))
	}
	name := find(t, got, detection.TypeFullName)
	pass := find(t, got, detection.TypePassportNumber)
	if !name.Personal {
		t.Errorf("name personal = false, want true (type allowed)")
	}
	if pass.Personal {
		t.Errorf("passport personal = true, want false (type excluded)")
	}
	if !hasReason(pass.ReasonCodes, ReasonTypeDisabledByPolicy) {
		t.Errorf("passport reasons %v missing type_disabled_by_policy", pass.ReasonCodes)
	}
	if pass.OwnerType != OwnerTypePerson {
		t.Errorf("passport owner type = %s, want PERSON preserved", pass.OwnerType)
	}
	if pass.OwnerID == "" || pass.OwnerID != name.OwnerID {
		t.Errorf("passport owner id %q != name owner id %q, want preserved shared id", pass.OwnerID, name.OwnerID)
	}
}

func TestClassificationOnlyMarkersAmbiguous(t *testing.T) {
	tests := []struct {
		text string
		sub  string
		typ  detection.Type
	}{
		{"родился 01.02.1990", "01.02.1990", detection.TypeBirthDate},
		{"родилась 01.02.1990", "01.02.1990", detection.TypeBirthDate},
		{"адрес проживания: Москва", "Москва", detection.TypeAddress},
		{"проживает в Москве", "Москве", detection.TypeAddress},
		{"зарегистрирован в Москве", "Москве", detection.TypeAddress},
	}
	for _, tt := range tests {
		start := idx(t, tt.text, tt.sub)
		in := []merge.Entity{ent(tt.typ, start, start+len(tt.sub), 0.9, detection.SourceRubert)}
		got := Assess(tt.text, in)
		if len(got) != 1 {
			t.Fatalf("Assess(%q) = %d results, want 1", tt.text, len(got))
		}
		e := got[0]
		if e.Personal {
			t.Errorf("Assess(%q) personal = true, want false (classification-only)", tt.text)
		}
		if e.OwnerType != OwnerTypeUnknown {
			t.Errorf("Assess(%q) owner type = %s, want UNKNOWN", tt.text, e.OwnerType)
		}
		if !e.ReviewRecommended {
			t.Errorf("Assess(%q) review recommended = false, want true", tt.text)
		}
		if hasReason(e.ReasonCodes, ReasonPositiveContext) {
			t.Errorf("Assess(%q) reasons = %v, want no ReasonPositiveContext", tt.text, e.ReasonCodes)
		}
	}
}

func TestNegatedPositiveMarkersNotPersonal(t *testing.T) {
	tests := []struct {
		text string
		sub  string
		typ  detection.Type
	}{
		{"это не дата рождения: 01.02.1990", "01.02.1990", detection.TypeBirthDate},
		{"это не был адрес регистрации: Москва", "Москва", detection.TypeAddress},
		{"это никогда не была дата рождения: 01.02.1990", "01.02.1990", detection.TypeBirthDate},
		{"это никогда не был адрес регистрации: Москва", "Москва", detection.TypeAddress},
	}
	for _, tt := range tests {
		start := idx(t, tt.text, tt.sub)
		in := []merge.Entity{ent(tt.typ, start, start+len(tt.sub), 0.9, detection.SourceRubert)}
		got := Assess(tt.text, in)
		if len(got) != 1 {
			t.Fatalf("Assess(%q) = %d results, want 1", tt.text, len(got))
		}
		e := got[0]
		if e.Personal {
			t.Errorf("Assess(%q) personal = true, want false (negated marker)", tt.text)
		}
		if e.OwnerType != OwnerTypeUnknown {
			t.Errorf("Assess(%q) owner type = %s, want UNKNOWN", tt.text, e.OwnerType)
		}
		if !e.ReviewRecommended {
			t.Errorf("Assess(%q) review recommended = false, want true", tt.text)
		}
		if hasReason(e.ReasonCodes, ReasonPositiveContext) {
			t.Errorf("Assess(%q) reasons = %v, want no ReasonPositiveContext", tt.text, e.ReasonCodes)
		}
	}
}

func TestPositiveMarkerAfterClauseBoundary(t *testing.T) {
	tests := []struct {
		text string
		sub  string
		typ  detection.Type
	}{
		{"это не дата встречи. дата рождения: 01.02.1990", "01.02.1990", detection.TypeBirthDate},
		{"это не адрес офиса; адрес регистрации: Москва", "Москва", detection.TypeAddress},
	}
	for _, tt := range tests {
		start := idx(t, tt.text, tt.sub)
		in := []merge.Entity{ent(tt.typ, start, start+len(tt.sub), 0.9, detection.SourceRubert)}
		got := Assess(tt.text, in)
		if len(got) != 1 {
			t.Fatalf("Assess(%q) = %d results, want 1", tt.text, len(got))
		}
		e := got[0]
		if !e.Personal {
			t.Errorf("Assess(%q) personal = false, want true", tt.text)
		}
		if e.OwnerType != OwnerTypePerson {
			t.Errorf("Assess(%q) owner type = %s, want PERSON", tt.text, e.OwnerType)
		}
		if !hasReason(e.ReasonCodes, ReasonPositiveContext) {
			t.Errorf("Assess(%q) reasons = %v, want ReasonPositiveContext", tt.text, e.ReasonCodes)
		}
	}
}

func TestOrganizationOverridesPositiveMarker(t *testing.T) {
	text := "ООО Ромашка, юридический адрес: Москва, ул. Ленина, д. 5"
	addrStart := idx(t, text, "Москва, ул. Ленина, д. 5")
	in := []merge.Entity{
		ent(detection.TypeAddress, addrStart, addrStart+len("Москва, ул. Ленина, д. 5"), 0.9, detection.SourceRubert),
	}
	got := Assess(text, in)
	if len(got) != 1 {
		t.Fatalf("Assess() = %d results, want 1", len(got))
	}
	e := got[0]
	if e.Personal {
		t.Errorf("personal = true, want false (organization overrides positive marker)")
	}
	if e.OwnerType != OwnerTypeOrganization {
		t.Errorf("owner type = %s, want ORGANIZATION", e.OwnerType)
	}
}

func TestComponentNameEvidenceLinksEntity(t *testing.T) {
	text := "Иван, email: ivan@example.com"
	nameStart := idx(t, text, "Иван")
	emailStart := idx(t, text, "ivan@example.com")
	nameEnd := nameStart + len("Иван")
	emailEnd := emailStart + len("ivan@example.com")

	city := merge.Entity{
		Candidate: detection.Candidate{
			Type: detection.TypeAddressCity, Start: nameStart, End: nameEnd,
			Confidence: 0.95, Sources: []detection.Source{detection.SourceRubert},
		},
		Components: []detection.Candidate{
			{Type: detection.TypeFullName, Start: nameStart, End: nameEnd,
				Confidence: 0.8, Sources: []detection.Source{detection.SourceGliner}},
		},
	}
	email := merge.Entity{
		Candidate: detection.Candidate{
			Type: detection.TypeEmail, Start: emailStart, End: emailEnd,
			Confidence: 0.9, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator},
		},
	}

	got := Assess(text, []merge.Entity{city, email})
	if len(got) != 2 {
		t.Fatalf("Assess() = %d results, want 2", len(got))
	}
	for _, e := range got {
		if !e.Personal {
			t.Errorf("entity %s personal = false, want true", e.Type)
		}
		if e.OwnerType != OwnerTypePerson {
			t.Errorf("entity %s owner type = %s, want PERSON", e.Type, e.OwnerType)
		}
		if !hasReason(e.ReasonCodes, ReasonLinkedEntities) {
			t.Errorf("entity %s reasons = %v, want ReasonLinkedEntities", e.Type, e.ReasonCodes)
		}
	}
}

func TestSeparateParagraphsDoNotShareContextOrOwner(t *testing.T) {
	text := "Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ, паспорт 00 00 000000\nПетров Пётр Петрович\nКлиент СИДОРОВ СИДОР СИДОРОВИЧ, паспорт 11 11 111111"
	name1Start := idx(t, text, "ТЕСТОВ ТЕСТ ТЕСТОВИЧ")
	pass1Start := idx(t, text, "00 00 000000")
	name2Start := idx(t, text, "Петров Пётр Петрович")
	name3Start := idx(t, text, "СИДОРОВ СИДОР СИДОРОВИЧ")
	pass3Start := idx(t, text, "11 11 111111")
	in := []merge.Entity{
		ent(detection.TypeFullName, name1Start, name1Start+len("ТЕСТОВ ТЕСТ ТЕСТОВИЧ"), 0.95, detection.SourceRubert),
		ent(detection.TypePassportNumber, pass1Start, pass1Start+len("00 00 000000"), 1.0, detection.SourceRegex),
		ent(detection.TypeFullName, name2Start, name2Start+len("Петров Пётр Петрович"), 0.95, detection.SourceRubert),
		ent(detection.TypeFullName, name3Start, name3Start+len("СИДОРОВ СИДОР СИДОРОВИЧ"), 0.95, detection.SourceRubert),
		ent(detection.TypePassportNumber, pass3Start, pass3Start+len("11 11 111111"), 1.0, detection.SourceRegex),
	}

	got := Assess(text, in)
	if len(got) != 5 {
		t.Fatalf("Assess() = %d results, want 5", len(got))
	}

	// Paragraph 1: positive person block shares one owner id.
	name1 := find(t, got, detection.TypeFullName)
	pass1 := find(t, got, detection.TypePassportNumber)
	if !name1.Personal || !pass1.Personal {
		t.Errorf("paragraph 1 entities not personal")
	}
	if name1.OwnerID == "" || name1.OwnerID != pass1.OwnerID {
		t.Errorf("paragraph 1 owner ids %q/%q, want same non-empty", name1.OwnerID, pass1.OwnerID)
	}

	// Paragraph 2: ambiguous name must not inherit paragraph 1 context.
	var name2 *Entity
	for i := range got {
		if got[i].Type == detection.TypeFullName && got[i].Start == name2Start {
			name2 = &got[i]
		}
	}
	if name2 == nil {
		t.Fatalf("second paragraph name not found")
	}
	if name2.Personal {
		t.Errorf("paragraph 2 name personal = true, want false (no shared context)")
	}
	if name2.OwnerType != OwnerTypeUnknown {
		t.Errorf("paragraph 2 owner type = %s, want UNKNOWN", name2.OwnerType)
	}
	if !name2.ReviewRecommended {
		t.Errorf("paragraph 2 review recommended = false, want true")
	}
	if name2.OwnerID != "" {
		t.Errorf("paragraph 2 owner id = %q, want empty", name2.OwnerID)
	}

	// Paragraph 3: independent positive person block shares its own owner id,
	// distinct from paragraph 1, in document order (person-1, person-2).
	var name3 *Entity
	var pass3 *Entity
	for i := range got {
		switch {
		case got[i].Type == detection.TypeFullName && got[i].Start == name3Start:
			name3 = &got[i]
		case got[i].Type == detection.TypePassportNumber && got[i].Start == pass3Start:
			pass3 = &got[i]
		}
	}
	if name3 == nil || pass3 == nil {
		t.Fatalf("third paragraph entities not found")
	}
	if !name3.Personal || !pass3.Personal {
		t.Errorf("paragraph 3 entities not personal")
	}
	if name3.OwnerID == "" || name3.OwnerID != pass3.OwnerID {
		t.Errorf("paragraph 3 owner ids %q/%q, want same non-empty", name3.OwnerID, pass3.OwnerID)
	}
	if name3.OwnerID == name1.OwnerID {
		t.Errorf("paragraph 3 owner id %q equals paragraph 1 owner id, want distinct", name3.OwnerID)
	}
	if name1.OwnerID != "person-1" || name3.OwnerID != "person-2" {
		t.Errorf("owner ids = %q/%q, want person-1/person-2 in document order", name1.OwnerID, name3.OwnerID)
	}
}

func TestInputOrderIndependenceAndDeterminism(t *testing.T) {
	text := "Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ, паспорт 00 00 000000"
	nameStart := idx(t, text, "ТЕСТОВ ТЕСТ ТЕСТОВИЧ")
	passStart := idx(t, text, "00 00 000000")
	base := []merge.Entity{
		ent(detection.TypeFullName, nameStart, nameStart+len("ТЕСТОВ ТЕСТ ТЕСТОВИЧ"), 0.95, detection.SourceRubert),
		ent(detection.TypePassportNumber, passStart, passStart+len("00 00 000000"), 1.0, detection.SourceRegex),
	}
	reversed := []merge.Entity{
		ent(detection.TypePassportNumber, passStart, passStart+len("00 00 000000"), 1.0, detection.SourceRegex),
		ent(detection.TypeFullName, nameStart, nameStart+len("ТЕСТОВ ТЕСТ ТЕСТОВИЧ"), 0.95, detection.SourceRubert),
	}

	want := Assess(text, base)
	got := Assess(text, reversed)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("input order changed result:\n got %+v\nwant %+v", got, want)
	}

	for i := range got {
		if got[i].OwnershipScore < 0 || got[i].OwnershipScore > 1 {
			t.Errorf("score %v out of bounds", got[i].OwnershipScore)
		}
		if !isSortedReasons(got[i].ReasonCodes) {
			t.Errorf("reasons %v not canonically ordered", got[i].ReasonCodes)
		}
	}
}

func TestDeepNoAliasing(t *testing.T) {
	text := "Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ, паспорт 00 00 000000"
	nameStart := idx(t, text, "ТЕСТОВ ТЕСТ ТЕСТОВИЧ")
	passStart := idx(t, text, "00 00 000000")
	src := []detection.Source{detection.SourceRubert, detection.SourceRegex}
	comp := detection.Candidate{Type: detection.TypeFirstName, Start: nameStart, End: nameStart + 6, Confidence: 0.9, Sources: []detection.Source{detection.SourceRubert}}
	in := []merge.Entity{
		{
			Candidate:  detection.Candidate{Type: detection.TypeFullName, Start: nameStart, End: nameStart + len("ТЕСТОВ ТЕСТ ТЕСТОВИЧ"), Confidence: 0.95, Sources: src},
			Components: []detection.Candidate{comp},
		},
		ent(detection.TypePassportNumber, passStart, passStart+len("00 00 000000"), 1.0, detection.SourceRegex),
	}

	orig := make([]merge.Entity, len(in))
	for i, e := range in {
		orig[i] = e
		orig[i].Sources = append([]detection.Source(nil), e.Sources...)
		if e.Components != nil {
			orig[i].Components = make([]detection.Candidate, len(e.Components))
			for j, c := range e.Components {
				orig[i].Components[j] = c
				orig[i].Components[j].Sources = append([]detection.Source(nil), c.Sources...)
			}
		}
	}

	got := Assess(text, in)
	if !reflect.DeepEqual(in, orig) {
		t.Errorf("Assess() mutated input: got %+v, want %+v", in, orig)
	}

	got[0].Sources[0] = detection.SourceValidator
	got[0].Components[0].Sources[0] = detection.SourceValidator
	if !reflect.DeepEqual(in, orig) {
		t.Errorf("mutating result aliased input: got %+v, want %+v", in, orig)
	}
}

func TestEmptyAndNilInput(t *testing.T) {
	if got := Assess("", nil); got != nil {
		t.Errorf("Assess(nil) = %+v, want nil", got)
	}
	if got := Assess("", []merge.Entity{}); got != nil {
		t.Errorf("Assess(empty) = %+v, want nil", got)
	}
}

func TestApplyPolicyAllowsTypeUnchanged(t *testing.T) {
	text := "Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ, паспорт 00 00 000000"
	nameStart := idx(t, text, "ТЕСТОВ ТЕСТ ТЕСТОВИЧ")
	passStart := idx(t, text, "00 00 000000")
	in := []merge.Entity{
		ent(detection.TypeFullName, nameStart, nameStart+len("ТЕСТОВ ТЕСТ ТЕСТОВИЧ"), 0.95, detection.SourceRubert),
		ent(detection.TypePassportNumber, passStart, passStart+len("00 00 000000"), 1.0, detection.SourceRegex),
	}
	assessed := Assess(text, in)
	p := policy.NewPolicy([]string{string(detection.TypeFullName), string(detection.TypePassportNumber)})

	got := ApplyPolicy(assessed, p)
	if len(got) != 2 {
		t.Fatalf("ApplyPolicy() = %d results, want 2", len(got))
	}
	for i := range got {
		if !got[i].Personal {
			t.Errorf("entity %s personal = false, want true (type allowed)", got[i].Type)
		}
		if hasReason(got[i].ReasonCodes, ReasonTypeDisabledByPolicy) {
			t.Errorf("entity %s has type_disabled_by_policy, want none", got[i].Type)
		}
	}
	if !reflect.DeepEqual(got, assessed) {
		t.Errorf("ApplyPolicy(allowed) changed result:\n got %+v\nwant %+v", got, assessed)
	}
}

func TestApplyPolicyExcludesType(t *testing.T) {
	text := "Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ, паспорт 00 00 000000"
	nameStart := idx(t, text, "ТЕСТОВ ТЕСТ ТЕСТОВИЧ")
	passStart := idx(t, text, "00 00 000000")
	in := []merge.Entity{
		ent(detection.TypeFullName, nameStart, nameStart+len("ТЕСТОВ ТЕСТ ТЕСТОВИЧ"), 0.95, detection.SourceRubert),
		ent(detection.TypePassportNumber, passStart, passStart+len("00 00 000000"), 1.0, detection.SourceRegex),
	}
	assessed := Assess(text, in)
	p := policy.NewPolicy([]string{string(detection.TypeFullName)})

	got := ApplyPolicy(assessed, p)
	if len(got) != 2 {
		t.Fatalf("ApplyPolicy() = %d results, want 2", len(got))
	}

	name := find(t, got, detection.TypeFullName)
	pass := find(t, got, detection.TypePassportNumber)
	if !name.Personal {
		t.Errorf("name personal = false, want true (type allowed)")
	}
	if pass.Personal {
		t.Errorf("passport personal = true, want false (type excluded)")
	}
	if !hasReason(pass.ReasonCodes, ReasonTypeDisabledByPolicy) {
		t.Errorf("passport reasons %v missing type_disabled_by_policy", pass.ReasonCodes)
	}
	if hasReason(name.ReasonCodes, ReasonTypeDisabledByPolicy) {
		t.Errorf("name reasons %v has type_disabled_by_policy, want none", name.ReasonCodes)
	}
	if pass.OwnerType != OwnerTypePerson {
		t.Errorf("passport owner type = %s, want PERSON preserved", pass.OwnerType)
	}
	if pass.OwnerID == "" || pass.OwnerID != name.OwnerID {
		t.Errorf("passport owner id %q != name owner id %q, want preserved shared id", pass.OwnerID, name.OwnerID)
	}
	if pass.OwnershipScore != assessed[findIdx(assessed, detection.TypePassportNumber)].OwnershipScore {
		t.Errorf("passport ownership score changed by policy")
	}
}

func TestApplyPolicyMixedEnabledDisabled(t *testing.T) {
	text := "Иванов Иван Иванович, телефон: +7 900 123-45-67, email: ivanov@example.com"
	nameStart := idx(t, text, "Иванов Иван Иванович")
	phoneStart := idx(t, text, "+7 900 123-45-67")
	emailStart := idx(t, text, "ivanov@example.com")
	in := []merge.Entity{
		ent(detection.TypeFullName, nameStart, nameStart+len("Иванов Иван Иванович"), 0.95, detection.SourceRubert),
		ent(detection.TypePhone, phoneStart, phoneStart+len("+7 900 123-45-67"), 0.9, detection.SourceRegex),
		ent(detection.TypeEmail, emailStart, emailStart+len("ivanov@example.com"), 0.95, detection.SourceRegex),
	}
	assessed := Assess(text, in)
	p := policy.NewPolicy([]string{string(detection.TypeFullName), string(detection.TypeEmail)})

	got := ApplyPolicy(assessed, p)
	if len(got) != 3 {
		t.Fatalf("ApplyPolicy() = %d results, want 3", len(got))
	}
	name := find(t, got, detection.TypeFullName)
	phone := find(t, got, detection.TypePhone)
	email := find(t, got, detection.TypeEmail)
	if !name.Personal || !email.Personal {
		t.Errorf("name/email personal = %v/%v, want true/true", name.Personal, email.Personal)
	}
	if phone.Personal {
		t.Errorf("phone personal = true, want false (type excluded)")
	}
	if !hasReason(phone.ReasonCodes, ReasonTypeDisabledByPolicy) {
		t.Errorf("phone reasons %v missing type_disabled_by_policy", phone.ReasonCodes)
	}
	if hasReason(name.ReasonCodes, ReasonTypeDisabledByPolicy) || hasReason(email.ReasonCodes, ReasonTypeDisabledByPolicy) {
		t.Errorf("allowed entities must not carry type_disabled_by_policy")
	}
}

func TestApplyPolicyLeavesNonPersonalUnchanged(t *testing.T) {
	text := "Александр Пушкин — русский поэт.\nОтделение банка находится по адресу: г. Москва, ул. Тестовая, д. 1\nВстреча состоится в городе Тестовск."
	poetStart := idx(t, text, "Александр Пушкин")
	addrStart := idx(t, text, "г. Москва, ул. Тестовая, д. 1")
	locStart := idx(t, text, "Тестовск")
	in := []merge.Entity{
		ent(detection.TypeFullName, poetStart, poetStart+len("Александр Пушкин"), 0.95, detection.SourceRubert),
		ent(detection.TypeAddress, addrStart, addrStart+len("г. Москва, ул. Тестовая, д. 1"), 0.9, detection.SourceGliner),
		ent(detection.TypeAddressCity, locStart, locStart+len("Тестовск"), 0.9, detection.SourceGliner),
	}
	assessed := Assess(text, in)
	p := policy.NewPolicy([]string{string(detection.TypeFullName)})

	got := ApplyPolicy(assessed, p)
	if len(got) != 3 {
		t.Fatalf("ApplyPolicy() = %d results, want 3", len(got))
	}
	for i := range got {
		if got[i].Personal {
			t.Errorf("entity %s personal = true, want false (non-personal)", got[i].Type)
		}
		if hasReason(got[i].ReasonCodes, ReasonTypeDisabledByPolicy) {
			t.Errorf("entity %s has type_disabled_by_policy, want none", got[i].Type)
		}
	}
	if !reflect.DeepEqual(got, assessed) {
		t.Errorf("ApplyPolicy changed non-personal results:\n got %+v\nwant %+v", got, assessed)
	}
}

func TestApplyPolicyDeterministicReasonOrder(t *testing.T) {
	text := "Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ, паспорт 00 00 000000"
	nameStart := idx(t, text, "ТЕСТОВ ТЕСТ ТЕСТОВИЧ")
	passStart := idx(t, text, "00 00 000000")
	in := []merge.Entity{
		ent(detection.TypeFullName, nameStart, nameStart+len("ТЕСТОВ ТЕСТ ТЕСТОВИЧ"), 0.95, detection.SourceRubert),
		ent(detection.TypePassportNumber, passStart, passStart+len("00 00 000000"), 1.0, detection.SourceRegex),
	}
	assessed := Assess(text, in)
	p := policy.NewPolicy([]string{string(detection.TypeFullName)})

	got := ApplyPolicy(assessed, p)
	pass := find(t, got, detection.TypePassportNumber)
	if !isSortedReasons(pass.ReasonCodes) {
		t.Errorf("passport reasons %v not canonically ordered", pass.ReasonCodes)
	}
	if !hasReason(pass.ReasonCodes, ReasonTypeDisabledByPolicy) {
		t.Errorf("passport reasons %v missing type_disabled_by_policy", pass.ReasonCodes)
	}
}

func TestApplyPolicyNoAliasing(t *testing.T) {
	text := "Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ, паспорт 00 00 000000"
	nameStart := idx(t, text, "ТЕСТОВ ТЕСТ ТЕСТОВИЧ")
	passStart := idx(t, text, "00 00 000000")
	src := []detection.Source{detection.SourceRubert, detection.SourceRegex}
	comp := detection.Candidate{Type: detection.TypeFirstName, Start: nameStart, End: nameStart + 6, Confidence: 0.9, Sources: []detection.Source{detection.SourceRubert}}
	in := []merge.Entity{
		{
			Candidate:  detection.Candidate{Type: detection.TypeFullName, Start: nameStart, End: nameStart + len("ТЕСТОВ ТЕСТ ТЕСТОВИЧ"), Confidence: 0.95, Sources: src},
			Components: []detection.Candidate{comp},
		},
		ent(detection.TypePassportNumber, passStart, passStart+len("00 00 000000"), 1.0, detection.SourceRegex),
	}
	assessed := Assess(text, in)
	orig := make([]Entity, len(assessed))
	for i, e := range assessed {
		orig[i] = copyEntityResult(e)
	}
	p := policy.NewPolicy([]string{string(detection.TypeFullName)})

	got := ApplyPolicy(assessed, p)
	if !reflect.DeepEqual(assessed, orig) {
		t.Errorf("ApplyPolicy() mutated input: got %+v, want %+v", assessed, orig)
	}

	got[0].Sources[0] = detection.SourceValidator
	got[0].Components[0].Sources[0] = detection.SourceValidator
	got[0].ReasonCodes[0] = ReasonAmbiguous
	if !reflect.DeepEqual(assessed, orig) {
		t.Errorf("mutating result aliased input: got %+v, want %+v", assessed, orig)
	}
}

func TestApplyPolicyEmptyAndNil(t *testing.T) {
	p := policy.NewPolicy(nil)
	if got := ApplyPolicy(nil, p); got != nil {
		t.Errorf("ApplyPolicy(nil) = %+v, want nil", got)
	}
	if got := ApplyPolicy([]Entity{}, p); got != nil {
		t.Errorf("ApplyPolicy(empty) = %+v, want nil", got)
	}
}

func findIdx(results []Entity, typ detection.Type) int {
	for i := range results {
		if results[i].Type == typ {
			return i
		}
	}
	return -1
}

func hasReason(reasons []ReasonCode, want ReasonCode) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

func isSortedReasons(reasons []ReasonCode) bool {
	pos := make(map[ReasonCode]int)
	for i, r := range reasonOrder {
		pos[r] = i
	}
	for i := 1; i < len(reasons); i++ {
		if pos[reasons[i-1]] > pos[reasons[i]] {
			return false
		}
	}
	return true
}
