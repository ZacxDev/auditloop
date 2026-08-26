package diff

import (
	"reflect"
	"testing"
)

func TestStringSetDelta(t *testing.T) {
	tests := []struct {
		name              string
		prev, cur         []string
		wantAdd, wantGone []string
	}{
		{
			name: "page set add and remove",
			prev: []string{"https://a/", "https://a/about", "https://a/old"},
			cur:  []string{"https://a/", "https://a/about", "https://a/new"},
			// added = in cur not prev; removed = in prev not cur.
			wantAdd:  []string{"https://a/new"},
			wantGone: []string{"https://a/old"},
		},
		{
			name:     "a11y new rule regression",
			prev:     []string{"color-contrast"},
			cur:      []string{"color-contrast", "label", "image-alt"},
			wantAdd:  []string{"image-alt", "label"}, // sorted
			wantGone: nil,
		},
		{
			name:     "all resolved",
			prev:     []string{"label", "image-alt"},
			cur:      []string{},
			wantAdd:  nil,
			wantGone: []string{"image-alt", "label"},
		},
		{
			name:     "identical sets",
			prev:     []string{"x", "y"},
			cur:      []string{"y", "x"},
			wantAdd:  nil,
			wantGone: nil,
		},
		{
			name:     "dedup and ignore empties",
			prev:     []string{"a", "a", ""},
			cur:      []string{"a", "b", "b", ""},
			wantAdd:  []string{"b"},
			wantGone: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			add, gone := StringSetDelta(tt.prev, tt.cur)
			if !reflect.DeepEqual(add, tt.wantAdd) {
				t.Errorf("added = %v, want %v", add, tt.wantAdd)
			}
			if !reflect.DeepEqual(gone, tt.wantGone) {
				t.Errorf("removed = %v, want %v", gone, tt.wantGone)
			}
		})
	}
}
