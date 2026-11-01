package httpapi

import (
	"strings"
	"testing"
)

func TestFilterProfanity(t *testing.T) {
	tests := []struct {
		input    string
		hasBad   bool
		expected string
	}{
		{"Привет, как дела?", false, "Привет, как дела?"},
		{"Колебания частоты и рубль за хлеб", false, "Колебания частоты и рубль за хлеб"},
		{"Какая сабля и гребля у космонавтов", false, "Какая сабля и гребля у космонавтов"},
		{"Пошел нахуй отсюда", true, "Пошел ****** отсюда"},
		{"Это полный пиздец", true, "Это полный ******"},
		{"Ну ты и долбоеб", true, "Ну ты и *******"},
		{"Блять, что происходит?", true, "***** что происходит?"},
		{"Ты сука", true, "Ты ****"},
		{"What the fuck is this", true, "What the **** is this"},
	}

	for _, tt := range tests {
		got := FilterProfanity(tt.input)
		if !tt.hasBad && got != tt.input {
			t.Errorf("False positive for %q: got %q", tt.input, got)
		}
		if tt.hasBad && !strings.Contains(got, "*") {
			t.Errorf("Failed to filter bad word in %q: got %q", tt.input, got)
		}
	}
}
