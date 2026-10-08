package atom

// ContainerState maps Docker and Podman process states to scheduler states.
func ContainerState(status string) State {
	switch status {
	case "created":
		return Created
	case "running":
		return Running
	case "removing":
		return Stopping
	case "exited", "dead":
		return Stopped
	default:
		return Invalid
	}
}

// ResultForExitCode maps a process exit code to the scheduler result.
func ResultForExitCode(code int) Result {
	switch code {
	case 0:
		return Success
	case 1:
		return Failure
	case 125, 126, 127:
		return StartupFailure
	case 137:
		return Killed
	case 143:
		return Terminated
	default:
		return Unknown
	}
}
