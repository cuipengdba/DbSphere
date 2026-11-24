package security

import "strings"

const InitialAdministrator = "admin"
const InitialAdministratorPassword = "1234567890123"

func NeedsInitialAdministratorPassword(password string) bool {
	return password == "" || password == InitialAdministratorPassword || password == "123456"
}

func ValidNewAdministratorPassword(password string) bool {
	return len(password) >= 12 && len(password) <= 256 && strings.TrimSpace(password) != "" &&
		password != InitialAdministratorPassword
}
