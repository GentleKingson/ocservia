package config

import "errors"

// FoundationDatabaseURL reuses secret-file validation for the development-only
// database tool without enabling a new Controller runtime backend.
func FoundationDatabaseURL(lookup LookupEnv) (string, error) {
	value, inline := lookup("OCSERV_DATABASE_URL")
	path, file := lookup("OCSERV_DATABASE_URL_FILE")
	if inline && file {
		return "", errors.New("OCSERV_DATABASE_URL and OCSERV_DATABASE_URL_FILE are mutually exclusive")
	}
	if file {
		var err error
		value, err = readBootstrapSecret(path)
		if err != nil {
			return "", errors.New("OCSERV_DATABASE_URL_FILE must be a private, launcher-owned secret file")
		}
	}
	if value == "" {
		return "", errors.New("OCSERV_DATABASE_URL or OCSERV_DATABASE_URL_FILE is required")
	}
	return value, nil
}
