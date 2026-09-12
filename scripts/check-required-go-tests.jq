# Exact names prevent a removed/renamed subtest from silently weakening the gate.
($manifest | split("\n") | map(select(length > 0 and (startswith("#") | not)) | split(" "))
 | map(select(length < 4 or .[3] == env.PR02_ENGINE)
   | select(.[0] == $group or
     ($group == "backend-mysql-full" and
       (.[0] == "backend-mysql-current" or .[0] == "backend-audit-mysql")) or
     ($group == "backend-mysql-full" and .[0] == "backend-mysql-history"))
   | {package: ("github.com/GentleKingson/ocservia/control-plane/" + .[1]), test: .[2]})) as $required
| if ($ARGS.named.mode // "check") == "select" then
    # Only top-level sets or children of ONE parent are supported. Do not
    # generate per-level unions for unrelated parents (a cross product).
    def literal: split("") | map(. as $c | if ("\\.^$|?*+()[]{}" | contains($c)) then "\\" + $c else $c end) | join("");
    def alternatives: unique | map(literal) | "^(" + join("|") + ")$";
    [$required[].package] | unique as $packages
    | [$required[].test] | unique as $names
    | [$names[] | select(contains("/") | not)] as $tops
    | if ($required | length) == 0 then error("empty required test group: " + $group)
      elif ($packages | length) != 1 then error("selection requires one package")
      elif all($names[]; split("/")[0] as $top | $tops | index($top)) then
        [$packages[0], ($tops | alternatives)] | join("\t")
      elif all($names[]; split("/") | length == 2) and
           ([$names[] | split("/")[0]] | unique | length) == 1 then
        [$packages[0], (($names[0] | split("/")[0] | literal | "^" + . + "$") + "/" +
          ([$names[] | split("/")[1]] | alternatives))] | join("\t")
      else error("selection requires top-level tests or children of one parent")
      end
  else
  . as $events
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
  end
