package task

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeRepo struct {
	calls int
	scope Scope
	in    Input
	id    int64
	err   error
	page  Page
	list  []Task
}

func (f *fakeRepo) List(_ context.Context, s Scope, p Page) ([]Task, error) {
	f.calls++
	f.scope = s
	f.page = p
	return f.list, f.err
}
func (f *fakeRepo) Create(_ context.Context, s Scope, in Input) (Task, error) {
	f.calls++
	f.scope = s
	f.in = in
	return Task{ID: 7, Name: in.Name, Status: in.Status}, f.err
}
func (f *fakeRepo) Update(_ context.Context, s Scope, id int64, in Input) (Task, error) {
	f.id = id
	return f.Create(context.Background(), s, in)
}
func (f *fakeRepo) Delete(_ context.Context, s Scope, id int64) error {
	f.calls++
	f.scope = s
	f.id = id
	return f.err
}

func TestInputContract(t *testing.T) {
	cases := []struct {
		name  string
		in    Input
		valid bool
	}{
		{"incomplete", Input{"first", 0, "", "", 0}, true}, {"complete", Input{"second", 1, "", "", 0}, true},
		{"unicode", Input{"待辦事項", 0, "", "", 0}, true}, {"boundary", Input{strings.Repeat("任", 1000), 1, "", "", 0}, true},
		{"preserve spaces", Input{"  todo  ", 1, "", "", 0}, true}, {"empty", Input{"", 0, "", "", 0}, false},
		{"blank", Input{" \n\t\u2003", 0, "", "", 0}, false}, {"too long", Input{strings.Repeat("任", 1001), 1, "", "", 0}, false},
		{"negative", Input{"task", -1, "", "", 0}, false}, {"status two", Input{"task", 2, "", "", 0}, false},
		{"null byte", Input{"a\x00b", 0, "", "", 0}, false}, {"invalid utf8", Input{string([]byte{0xff}), 0, "", "", 0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRepo{}
			s := NewService(f)
			scope := Scope{UserID: 21, TenantID: 31}
			result, err := s.Create(context.Background(), scope, tc.in)
			if tc.valid {
				if err != nil || f.calls != 1 || result.Name != tc.in.Name || f.scope != scope {
					t.Fatalf("valid create: result=%+v err=%v calls=%d", result, err, f.calls)
				}
			} else {
				if !errors.Is(err, ErrInvalid) || f.calls != 0 {
					t.Fatalf("invalid create reached repository: err=%v calls=%d", err, f.calls)
				}
			}
			f.calls = 0
			update := tc.in
			update.Version = 1
			_, err = s.Update(context.Background(), scope, 9, update)
			if tc.valid {
				if err != nil || f.calls != 1 || f.id != 9 {
					t.Fatalf("valid update: %v", err)
				}
			} else if !errors.Is(err, ErrInvalid) || f.calls != 0 {
				t.Fatalf("invalid update: err=%v calls=%d", err, f.calls)
			}
		})
	}
}
func TestServiceIDsAndErrors(t *testing.T) {
	f := &fakeRepo{err: ErrForbidden}
	s := NewService(f)
	ctx := context.Background()
	scope := Scope{UserID: 1, TenantID: 2}
	for _, id := range []int64{0, -1} {
		if err := s.Delete(ctx, scope, id); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		if _, err := s.Update(ctx, scope, id, Input{"ok", 0, "", "", 0}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if f.calls != 0 {
		t.Fatal("invalid IDs called repository")
	}
	if _, _, err := s.List(ctx, scope, Page{}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, scope, Input{"valid", 0, "", "", 0}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, scope, 1, Input{"valid", 1, "", "", 1}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, scope, 1); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}

func TestUpdateRequiresVersion(t *testing.T) {
	for _, version := range []int64{0, -1} {
		f := &fakeRepo{}
		_, err := NewService(f).Update(context.Background(), Scope{UserID: 1, TenantID: 2}, 1, Input{Name: "valid", Status: 0, Version: version})
		if !errors.Is(err, ErrInvalid) || f.calls != 0 {
			t.Fatalf("version %d reached repository: %v %d", version, err, f.calls)
		}
	}
}

func TestListPagination(t *testing.T) {
	ctx := context.Background()
	scope := Scope{UserID: 1, TenantID: 2}
	three := []Task{{ID: 10, Name: "a", Status: 0}, {ID: 11, Name: "b", Status: 1}, {ID: 12, Name: "c", Status: 0}}

	// Invalid pages never reach the repository.
	for _, p := range []Page{{Limit: -1}, {Limit: MaxPageSize + 1}, {After: -1}} {
		f := &fakeRepo{list: three}
		if _, _, err := NewService(f).List(ctx, scope, p); !errors.Is(err, ErrInvalid) || f.calls != 0 {
			t.Fatalf("page %+v: err=%v calls=%d", p, err, f.calls)
		}
	}

	// Omitted limit uses the bounded default; this short fixture needs no cursor.
	f := &fakeRepo{list: three}
	got, next, err := NewService(f).List(ctx, scope, Page{After: 5})
	if err != nil || len(got) != 3 || next != 0 || f.page != (Page{After: 5, Limit: 100}) || f.scope != scope {
		t.Fatalf("no limit: %v %d %+v %+v", err, next, got, f.page)
	}

	// Limit 2 with 3 rows available (repository returns limit+1): trim and point at the last returned id.
	f = &fakeRepo{list: three}
	got, next, err = NewService(f).List(ctx, scope, Page{Limit: 2})
	if err != nil || len(got) != 2 || got[1].ID != 11 || next != 11 || f.page != (Page{Limit: 2}) {
		t.Fatalf("limit 2: %v %d %+v", err, next, got)
	}

	// Limit 2 with exactly 2 rows: last page, no cursor.
	f = &fakeRepo{list: three[:2]}
	got, next, err = NewService(f).List(ctx, scope, Page{Limit: 2, After: 9})
	if err != nil || len(got) != 2 || next != 0 {
		t.Fatalf("last page: %v %d %+v", err, next, got)
	}

	// Limit at the maximum is valid.
	f = &fakeRepo{list: three}
	if _, _, err = NewService(f).List(ctx, scope, Page{Limit: MaxPageSize}); err != nil || f.calls != 1 {
		t.Fatalf("max page: %v", err)
	}
}

func TestDetailContract(t *testing.T) {
	ctx := context.Background()
	scope := Scope{UserID: 1, TenantID: 2}
	cases := []struct {
		name   string
		detail string
		valid  bool
	}{
		{"empty", "", true}, {"text", "buy oolong, not green", true},
		{"unicode max", strings.Repeat("細", MaxDetailLength), true},
		{"whitespace only is allowed", "   ", true},
		{"too long", strings.Repeat("細", MaxDetailLength+1), false},
		{"null byte", "a\x00b", false}, {"invalid utf8", string([]byte{0xfe}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRepo{}
			_, err := NewService(f).Create(ctx, scope, Input{"task", 0, tc.detail, "", 0})
			if tc.valid {
				if err != nil || f.calls != 1 || f.in.Detail != tc.detail {
					t.Fatalf("valid detail rejected: err=%v calls=%d", err, f.calls)
				}
			} else if !errors.Is(err, ErrInvalid) || f.calls != 0 {
				t.Fatalf("invalid detail reached repository: err=%v calls=%d", err, f.calls)
			}
		})
	}
}
