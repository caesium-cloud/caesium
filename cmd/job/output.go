package job

import "fmt"

func writeCmdOut(cmd *cobra.Command, format string, args ...any) error {
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), format, args...); err != nil {
		cmd.PrintErrf("write output: %v\n", err)
		return err
	}
	return nil
}
