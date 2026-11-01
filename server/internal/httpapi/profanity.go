package httpapi

import (
	"regexp"
	"strings"
	"unicode"
)

// Profanity regex patterns compiled once
var russianBadWordPatterns = []*regexp.Regexp{
	// хуй / нахуй / похуй / охуел / охуеть
	regexp.MustCompile(`(?i)(?:^|[^а-яa-z0-9])([а-яa-z0-9]*ху[йеёяиюjyea][а-яa-z0-9]*)(?:$|[^а-яa-z0-9])`),
	// пизд / пиздец / спиздил / распиздяй
	regexp.MustCompile(`(?i)(?:^|[^а-яa-z0-9])([а-яa-z0-9]*пизд[а-яa-z0-9]*)(?:$|[^а-яa-z0-9])`),
	// еб / ёб / выеб / наеб / заеб / ебать / долбоеб
	regexp.MustCompile(`(?i)(?:^|[^а-яa-z0-9])([а-яa-z0-9]*[её]б[а-яa-z0-9]+|долбо[её]б[а-яa-z0-9]*|[её]бл[а-яa-z0-9]*)(?:$|[^а-яa-z0-9])`),
	// бля / бляд / блять
	regexp.MustCompile(`(?i)(?:^|[^а-яa-z0-9])([а-яa-z0-9]*бл[яе]д[а-яa-z0-9]*|бл[яе]т[ьэ]|бля)(?:$|[^а-яa-z0-9])`),
	// сука / сучка / сучара
	regexp.MustCompile(`(?i)(?:^|[^а-яa-z0-9])(сук[аиоеу]|сучк[аиоеу]|сучар[а-яa-z0-9]*)(?:$|[^а-яa-z0-9])`),
	// пидор / пидорас / педик
	regexp.MustCompile(`(?i)(?:^|[^а-яa-z0-9])([а-яa-z0-9]*пид[аое]р[а-яa-z0-9]*|педик[а-яa-z0-9]*)(?:$|[^а-яa-z0-9])`),
	// говно / говнюк
	regexp.MustCompile(`(?i)(?:^|[^а-яa-z0-9])([а-яa-z0-9]*г[ао]вн[а-яa-z0-9]+)(?:$|[^а-яa-z0-9])`),
	// мудак / мудила
	regexp.MustCompile(`(?i)(?:^|[^а-яa-z0-9])(муд[аое]к[а-яa-z0-9]*|мудил[а-яa-z0-9]*)(?:$|[^а-яa-z0-9])`),
	// шлюха, гандон, залупа, дрочить
	regexp.MustCompile(`(?i)(?:^|[^а-яa-z0-9])(шлюх[а-яa-z0-9]*|г[ао]ндон[а-яa-z0-9]*|з[ао]луп[а-яa-z0-9]*|др[ао]ч[а-яa-z0-9]*)(?:$|[^а-яa-z0-9])`),
}

var englishBadWordPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(fuck[a-z0-9]*|fuk[a-z0-9]*|bitch[a-z0-9]*|shit[a-z0-9]*|asshole[a-z0-9]*|cunt[a-z0-9]*|dick[a-z0-9]*|bastard[a-z0-9]*)(?:$|[^a-z0-9])`),
}

// Исключения — благозвучные русские слова, содержащие корни вроде "еб" или "бля"
var innocentExceptions = []string{
	"колебан", "колебат", "колебле", "стебел", "стебл", "хлеб", "гребл", "рубл",
	"сабл", "корабл", "страхов", "употреб", "потреб", "оскорб", "влюбл", "скипидар",
	"педиатри", "педиатр", "космонавт", "шаролет", "шаролёт",
}

func isException(word string) bool {
	lower := strings.ToLower(word)
	for _, exc := range innocentExceptions {
		if strings.Contains(lower, exc) {
			return true
		}
	}
	return false
}

// normalizeLeetspeak приводит псевдо-латиницу и цифры к кириллице для устойчивости к обходу
func normalizeLeetspeak(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'a':
			sb.WriteRune('а')
		case 'b':
			sb.WriteRune('в')
		case 'c':
			sb.WriteRune('с')
		case 'e':
			sb.WriteRune('е')
		case 'k':
			sb.WriteRune('к')
		case 'm':
			sb.WriteRune('м')
		case 'o', '0':
			sb.WriteRune('о')
		case 'p':
			sb.WriteRune('р')
		case 't':
			sb.WriteRune('т')
		case 'x':
			sb.WriteRune('х')
		case 'y':
			sb.WriteRune('у')
		case '1', '!':
			sb.WriteRune('и')
		case '3':
			sb.WriteRune('з')
		case '4':
			sb.WriteRune('ч')
		case '@':
			sb.WriteRune('а')
		case '$':
			sb.WriteRune('с')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func maskMatches(text string, re *regexp.Regexp, masked []bool, runes []rune) {
	matches := re.FindAllStringSubmatchIndex(text, -1)
	for _, match := range matches {
		if len(match) >= 4 && match[2] >= 0 && match[3] >= 0 {
			startByte := match[2]
			endByte := match[3]

			startRune := len([]rune(text[:startByte]))
			endRune := len([]rune(text[:endByte]))

			matchedWord := string(runes[startRune:endRune])
			if isException(matchedWord) {
				continue
			}

			for i := startRune; i < endRune && i < len(masked); i++ {
				masked[i] = true
			}
		}
	}
}

// FilterProfanity заменяет нецензурную брань и мат звёздочками
func FilterProfanity(text string) string {
	if text == "" {
		return ""
	}

	runes := []rune(text)
	masked := make([]bool, len(runes))

	// 1. Проверяем английские ругательства на исходном тексте
	for _, re := range englishBadWordPatterns {
		maskMatches(text, re, masked, runes)
	}

	// 2. Проверяем русские ругательства на исходном тексте
	for _, re := range russianBadWordPatterns {
		maskMatches(text, re, masked, runes)
	}

	// 3. Проверяем русские ругательства на нормализованном leetspeak тексте
	normalized := normalizeLeetspeak(text)
	for _, re := range russianBadWordPatterns {
		maskMatches(normalized, re, masked, runes)
	}

	var result strings.Builder
	for i, r := range runes {
		if masked[i] && !unicode.IsSpace(r) {
			result.WriteRune('*')
		} else {
			result.WriteRune(r)
		}
	}
	return result.String()
}
