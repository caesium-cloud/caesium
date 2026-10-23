// API probes can pause between reads while the server closes an idle socket.
// Close each probe connection from its first request without retrying a send.
export function connectorProbeHeaders(headers: Record<string, string>): Record<string, string> {
  return { ...headers, Connection: "close" };
}
