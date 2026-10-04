// How one overview source should be read. A source that loaded once keeps
// its last values after a failed refresh, labelled stale instead of hidden.
export type SourceState = "loading" | "unavailable" | "stale" | "ready";

export function sourceState(
  loaded: boolean,
  unavailable: boolean,
): SourceState {
  if (!loaded) return unavailable ? "unavailable" : "loading";
  return unavailable ? "stale" : "ready";
}

export function sourceValue(state: SourceState, value: number): string {
  if (state === "loading") return "…";
  if (state === "unavailable") return "–";
  return String(value);
}
