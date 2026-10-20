package server

import "os"

// osGetpid reports the process id of the service.
func osGetpid() int { return os.Getpid() }
