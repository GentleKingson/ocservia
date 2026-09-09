# Exact names prevent a removed/renamed subtest from silently weakening the gate.
($manifest | split("\n") | map(select(length > 0 and (startswith("#") | not)) | split(" "))
 | map(select(.[0] == $group) | {package: ("github.com/GentleKingson/ocservia/control-plane/" + .[1]), test: .[2]})) as $required
| . as $events
| [$required[] | . as $want
   | [$events[] | select(.Package == $want.package and .Test == $want.test) | .Action] as $actions
   | select(($actions | index("run")) == null or ($actions | last) != "pass")
   | "\(.package) \(.test): missing run/final pass"] as $missing
| [$events[] | select(.Test != null and (.Action == "skip" or .Action == "fail"))
   | . as $event
   | select(any($required[]; . as $want | .package == $event.Package and
       (.test == $event.Test or ($event.Test | startswith($want.test + "/")))))
   | "\(.Package) \(.Test): \(.Action)"] as $bad
| if ($required | length) == 0 then error("empty required test group: " + $group)
  elif ($missing + $bad | length) > 0 then error(($missing + $bad) | join("\n"))
  else {group: $group, required: ($required | length), passed: ($required | length), skipped: 0}
  end
