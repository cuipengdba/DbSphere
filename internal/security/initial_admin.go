package security

import "strings"

const InitialAdministrator = "admin"
const InitialAdministratorPassword = "123456"

func NeedsInitialAdministratorPassword(password string) bool {
	return password == "" || password == InitialAdministratorPassword
}

func ValidNewAdministratorPassword(password string) bool {
	return len(password) >= 12 && len(password) <= 256 && strings.TrimSpace(password) != ""
}
