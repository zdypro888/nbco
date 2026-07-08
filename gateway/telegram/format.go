package telegram

import "github.com/zdypro888/nbco/textfmt/telegramhtml"

// toTelegramHTML keeps the gateway API local while sharing the actual
// formatter with chat persistence.
func toTelegramHTML(s string) string { return telegramhtml.ToHTML(s) }
