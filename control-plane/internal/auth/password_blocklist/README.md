# Offline Local password blocklist (R1, version 1)

`common-passwords.txt` is the unmodified decompressed list from Django 5.2.6,
commit `75c4403f07b8ad25893f7832dbe8fc6814b53b2d`:

- Source: https://github.com/django/django/blob/75c4403f07b8ad25893f7832dbe8fc6814b53b2d/django/contrib/auth/common-passwords.txt.gz
- Provenance: Django's `CommonPasswordValidator` credits Royce Williams's
  top-20,000 list derived from Troy Hunt's Pwned Passwords v6 (2020-06-19):
  https://gist.github.com/roycewilliams/226886fd01572964e1431ac8afc999ce
- Django unhexed, lowercased and deduplicated the data. This snapshot contains
  19,640 newline-delimited entries, including common and breached passwords.
- License: Django BSD-3-Clause; the full notice is in `LICENSE.django` and must
  accompany source and binary distributions that include this data.
- Decompressed SHA-256:
  `29ca0fa5303165f012f3e9775e3e95a3071cdd59f219973ec1cbb308d0214a6f`.

`service-passwords.txt` is an ocservia-authored supplement, version 1, maintained
under the repository license. It contains complete, obvious product/domain and
administrator/default-password candidates. It supplements, not replaces, the
upstream common/breached list.

Both lists are embedded in the binary. Checks compare the complete candidate
case-insensitively, without trimming or substring/dictionary-word matching.
Only the lookup copy is lowercased; password hashing uses the original bytes.
There are no online lookups, password uploads, runtime downloads or bypass flags.
This is a bounded blocklist, not a claim to contain every compromised password.

## Maintenance

Update by a normal reviewed repository change: select an immutable Django
release/commit, download its gzip list and license, decompress without editing
entries, and update the version, source, count and SHA-256 above. Review license
changes and list differences. Add concrete service-related whole candidates to
the supplement as needed and bump its version. Run the auth policy and Local
lifecycle tests on BuildServer, then ship the files with the normal binary build.
Do not add a separate password service or periodic forced password changes.
