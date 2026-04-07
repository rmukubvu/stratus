package apierror

type Error struct {
	StatusCode int
	Code       string
	Message    string
	Resource   string
}

func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}
