# ferret

**ferret** is a small command-line tool that runs HTTP API regression tests described in plain YAML files.
You describe requests, what to remember from the responses, and what must be true. ferret runs the
scenarios in order, records every call, and writes machine-readable results (JSON and JUnit) and a
self-contained HTML report.

It is a single static binary with no runtime dependencies. Adding or changing tests never needs a
rebuild: the tests are data, not code.

* [Why ferret](#why-ferret)
* [Install](#install)
* [Quick start](#quick-start)
* [Concepts](#concepts)
* [The suite file](#the-suite-file-suiteyaml)
* [Inputs and secrets](#inputs-and-secrets)
* [Scenario files](#scenario-files)
* [Steps](#steps)
* [Variables and templates](#variables-and-templates)
* [Captures](#captures)
* [Assertions](#assertions)
* [Dependencies, exports and matrices](#dependencies-exports-and-matrices)
* [Polling](#polling)
* [Cleanup steps](#cleanup-steps)
* [Command reference](#command-reference)
* [Results](#results)
* [Secret redaction](#secret-redaction)
* [Using ferret in CI](#using-ferret-in-ci)
* [Docker](#docker)
* [Generated test documentation](#generated-test-documentation)
* [Behavior to know about](#behavior-to-know-about)
* [Building from source](#building-from-source)
* [License](#license)

## Why ferret

* **Declarative.** Tests are YAML. A reviewer can read what is called and what is expected without
  reading code.
* **Strict.** Unknown keys, undefined variables, duplicate names and bad references are rejected
  before the first request is sent. A typo is an error, never a silently skipped check.
* **Workflow aware.** Scenarios are ordered workflows. A value captured from one response can feed
  later requests, scenarios can depend on each other, and a failure blocks what depends on it
  instead of producing a pile of misleading follow-on failures.
* **Safe by default.** Declared secrets are redacted from every log, result and report. Long
  base64 blobs are summarized so recorded bodies stay readable. Redirects are never followed, and
  polling is only allowed on `GET` so a mutating call is never retried.
* **Honest results.** A run with filters says it is partial. Statuses distinguish *failed*
  (a check did not hold) from *errored* (the check could not be evaluated) and *blocked*
  (an earlier step or dependency did not pass).

## Install

Requires Go 1.24 or newer to build from source.

```sh
go install github.com/unknovs/goferret/cmd/ferret@latest
```

Or clone the repository and build it:

```sh
git clone https://github.com/unknovs/goferret.git
cd goferret
go build -o ferret ./cmd/ferret
./ferret version
```

A container image can be built from the included `Dockerfile` (see [Docker](#docker)).

## Quick start

Create a folder with these files.

`suite.yaml`

```yaml
version: 1
name: Example API
inputs: inputs.yaml
timeoutSeconds: 30

tags: [smoke, items]

services:
  api:
    baseUrl: "{{API_URL}}"
    headers:
      Authorization: "Bearer {{API_TOKEN}}"

scenarios:
  - items.yaml
```

`inputs.yaml`

```yaml
version: 1
inputs:
  API_URL:
    type: url
    required: true
    description: Base URL of the API under test.
  API_TOKEN:
    required: true
    secret: true
    description: Bearer token.
```

`items.yaml`

```yaml
version: 1
scenarios:
  - id: items.create-and-read
    description: Create an item, then read it back
    tags: [smoke, items]
    vars:
      itemName: "item-{{$random_hex 8}}"
    steps:
      - id: create
        service: api
        method: POST
        path: /items
        json:
          name: "{{itemName}}"
          quantity: 3
        capture:
          itemId: { json: /id }
        assert:
          - name: Status code is 201
            checks:
              - { status: 201 }
          - name: Response describes the new item
            checks:
              - { json: /id, type: string }
              - { json: /name, eq: "{{itemName}}" }
              - { json: /quantity, eq: 3 }

      - id: read
        service: api
        method: GET
        path: /items/{{itemId}}
        assert:
          - name: Status code is 200
            checks:
              - { status: 200 }
          - name: The item can be read back
            checks:
              - { json: /id, eq: "{{itemId}}" }
              - { json: /name, eq: "{{itemName}}" }

    cleanup:
      - id: delete
        service: api
        method: DELETE
        path: /items/{{itemId}}
```

`.env` (do not commit this file)

```
API_URL=https://api.example.com
API_TOKEN=replace-me
```

Check the files, then run them:

```sh
ferret validate --tests suite.yaml --env-file .env
ferret run      --tests suite.yaml --env-file .env --output reports
```

Typical output:

```
PASS    items.create-and-read                    3 steps, 4 assertions, 0.4s

Assertions: total 4 | passed 4 | failed 0 | errored 0 | blocked 0
Scenarios:  total 1 | passed 1 | failed 0 | errored 0 | blocked 0
HTTP calls: 3 (including setup and helper calls)
Results written to reports (results.json, junit.xml)
```

Then render the report:

```sh
ferret report --input reports/results.json --output reports/report.html
```

## Concepts

| Term | Meaning |
| --- | --- |
| **Suite** | The entry file (`suite.yaml`). Names the services, the inputs contract, optional setup steps and the scenario files. |
| **Inputs** | Values supplied from outside (URLs, credentials, ids). Declared in `inputs.yaml`, never written into tests. |
| **Service** | A base URL plus default headers, referenced by name from steps. |
| **Scenario** | An ordered workflow of steps. Scenarios are independent unless one declares `dependsOn`. |
| **Step** | One HTTP request, with optional captures and assertions. |
| **Capture** | A value taken from a response and stored in a variable for later use. |
| **Assertion group** | A named set of checks on one response. Groups are what the totals count. |
| **Tag** | A label on scenarios used to select what runs. |
| **Fixture** | A file used as test data (a request payload, an upload). Kept under the fixture root. |

Outcomes use four statuses:

| Status | Meaning |
| --- | --- |
| `passed` | Every check held. |
| `failed` | A check was evaluated and did not hold. |
| `errored` | Something prevented evaluation: a transport error, a response that is not JSON when JSON was needed, an unresolved capture. |
| `blocked` | Not executed because an earlier step in the scenario, a dependency, or the suite setup did not pass. |

## The suite file (`suite.yaml`)

```yaml
version: 1                  # required, must be 1
name: Example API           # shown in results and reports
inputs: inputs.yaml         # required, path to the inputs contract, relative to this file
fixtureRoot: fixtures       # optional, folder for fixture files, relative to this file (default: this folder)
timeoutSeconds: 60          # optional default per-request timeout (default 60)

tags: [smoke, items, slow]  # every tag used by a scenario must be declared here
fullRunOnlyTags: [final]    # optional, see below

services:
  api:
    baseUrl: "{{API_URL}}"
    headers:                          # sent with every request to this service;
      Authorization: "Bearer {{API_TOKEN}}"   # an unset input is an error
    optionalHeaders:                  # sent only when every referenced input is set
      X-Trace-Id: "{{TRACE_ID}}"

setup:                      # optional steps that run once before any scenario
  - id: ready
    service: api
    method: GET
    path: /health
    poll: { attempts: 30, intervalSeconds: 2, until: [ { status: 200 } ] }
    assert:
      - name: API is ready
        checks: [ { status: 200 } ]

scenarios:                  # scenario files, relative to this file
  - items.yaml
```

Notes:

* Loading is strict: an unknown key anywhere is an error.
* If setup does not pass, every scenario is reported as `blocked`.
* Values captured in setup are visible to all scenarios.
* `fullRunOnlyTags` names tags for scenarios that only make sense in a complete run (for example a
  final "marker" scenario). A filtered run skips them unless you select them by `--scenario` or by
  their own tag.
* A step can opt out of its service's default headers with `noServiceHeaders: true`.

## Inputs and secrets

`inputs.yaml` is the contract of everything a suite needs from outside:

```yaml
version: 1
inputs:
  API_URL:
    type: url                 # string (default), url (http/https), or int
    required: true
    description: Base URL of the API under test.
  API_TOKEN:
    required: true
    secret: true              # redacted everywhere; cannot have a default
    description: Bearer token.
  REGION:
    default: eu-west          # used when nothing else supplies a value
  CLIENT_CERT:
    secret: true              # may be supplied as CLIENT_CERT_FILE
```

Input names use letters, digits and underscores, must not start with a digit, and must not end in
`_FILE` (that suffix is reserved).

### Where values come from

For each input, the first match wins, highest priority first:

1. the process environment: `NAME`
2. the process environment: `NAME_FILE` (a path to a file holding the value)
3. an `--env-file`: `NAME`
4. an `--env-file`: `NAME_FILE`
5. the `default` from `inputs.yaml`

Setting `NAME` and `NAME_FILE` in the same source is an error. An explicitly empty value counts as
set. File contents are used exactly as read, with no trimming, and must be valid UTF-8. The
`_FILE` form is the way to pass multi-line values and to consume mounted secrets.

Only inputs referenced by the selected scenarios (and setup) are required. A filtered run does not
demand credentials it will not use. Error messages name the input and where it was read from, never
the value.

### The env file

`--env-file` takes a strict dotenv file (repeatable; later files override earlier ones):

```
# comment
API_URL=https://api.example.com
API_TOKEN=abc123
MESSAGE="line one\nline two"   # double quotes allow \" \\ and \n
```

Rules: `KEY=value` lines and `#` comment lines only. Unquoted values are literal (no inline
comments, no expansion). There is no `export` prefix, no multi-line values, and no duplicate keys.
Every key must be a declared input (or its `_FILE` form), so a misspelled key is an error.

## Scenario files

```yaml
version: 1
scenarios:
  - id: items.create-and-read      # lowercase letters, digits, '.', '_' and '-'
    description: What this checks
    tags: [smoke, items]
    dependsOn: []                  # scenario ids that must pass first
    exports: []                    # variable names offered to dependents
    vars: {}                       # values computed once when the scenario starts
    matrix: []                     # expands one scenario into several
    steps: [ ... ]
    cleanup: [ ... ]
```

A scenario must have at least one step. Scenario ids must be unique across the whole suite.

## Steps

```yaml
- id: create                  # letters, digits, '_', '.', '-'; unique within the scenario
  description: Create an item # optional
  service: api                # a key from `services`
  method: POST                # GET, POST, PUT, PATCH, DELETE or HEAD
  path: /items/{{itemId}}     # starts with '/', appended to the service baseUrl
  query: { page: "1" }        # optional query parameters
  headers: { X-Mode: test }   # optional, added to the service headers
  noServiceHeaders: false     # true to skip the service's default headers
  timeoutSeconds: 10          # optional per-step timeout
  helper: false               # see below
  set: { today: "2026-01-01" }# variables assigned before the request is built
  json: { ... }               # one request body option, see below
  capture: { ... }
  assert: [ ... ]
  poll: { ... }
```

### Request bodies

At most one of these per step:

| Key | Sends |
| --- | --- |
| `json` | A JSON document. A string that is exactly one `"{{var}}"` keeps the variable's type, so a captured number or object is sent as a number or object, not as text. |
| `body` | Plain text (`text/plain; charset=utf-8`). |
| `form` | `application/x-www-form-urlencoded` fields. |
| `multipart` | `multipart/form-data` with `fields` (text) and `files` (form field → fixture path). |

```yaml
multipart:
  fields: { description: "{{note}}" }
  files:  { file: uploads/report.pdf }   # path under fixtureRoot
```

Fixture paths are always relative to `fixtureRoot` and may not escape it.

### Helper steps

`helper: true` marks a support call (obtaining a token, preparing data). Its checks run normally but
are counted separately from regression assertions in the totals, so support calls do not inflate
the regression numbers. Setup and cleanup steps are counted the same way.

## Variables and templates

Anywhere a string appears in a request (path, query, headers, bodies) or in an expectation, you can
write `{{name}}` or call a builtin as `{{$name arg}}`. There are no expressions or conditionals:
only variables and a small fixed set of builtins.

Variables are looked up in this order, and the first match wins:

1. the **scenario**: values assigned by step `set`, captures, the matrix entry, and the scenario's own `vars`,
2. **shared values**: exports from scenarios that already passed, and captures from setup,
3. **inputs**.

A variable that is not defined is an error, never an empty string. A reference to a name that can
never exist is reported by `ferret validate` before any request is sent. Scalars (strings, numbers,
booleans, null) can be interpolated into longer strings; objects and arrays can only be used as
a whole value.

### Builtins

| Builtin | Result |
| --- | --- |
| `{{$random_hex N}}` | `N` random hex characters, `1` to `64`. |
| `{{$file_base64 path}}` | Fixture file contents, base64 encoded. |
| `{{$file_sha256_b64 path}}` | SHA-256 of a fixture file, base64 encoded. |
| `{{$file_text path}}` | Fixture file contents as UTF-8 text. |
| `{{$base64_of name}}` | Base64 of the text value of the variable `name` (a variable name, not a literal). |
| `{{$utc_now_minus_hours N}}` | Current UTC time minus `N` hours (`0` to `8760`) as `YYYY-MM-DDTHH:MM:SSZ`. |

`vars` are rendered once when the scenario starts, which makes them the place for generated unique
values that several steps share:

```yaml
vars:
  email: "user-{{$random_hex 8}}@example.com"
```

## Captures

A capture stores a value from the response under a name for later steps and assertions. Exactly one
source per capture:

```yaml
capture:
  itemId:   { json: /id }               # a JSON Pointer into the response body
  etag:     { header: ETag }            # a response header (first value)
  code:     { status: true }            # the HTTP status code
  authToken: { json: /token, secret: true }   # marked secret: redacted everywhere
```

JSON locations use [JSON Pointer](https://www.rfc-editor.org/rfc/rfc6901) syntax: `/items/0/name`,
with `~1` for a literal `/` and `~0` for a literal `~`. A capture whose location is missing makes
the step `errored`. Captured values are never written to `results.json`; only the names are listed.

## Assertions

A step's `assert` is a list of named groups. Each group holds one or more `checks`, and all of them
are evaluated even when an earlier one fails, so one run shows every problem at once. A group's name
must be unique within the step.

```yaml
assert:
  - name: Status code is 200
    checks:
      - { status: 200 }
  - name: Body has the expected shape
    checks:
      - { json: /id, type: string }
      - { json: /tags, type: array, lengthMin: 1 }
      - { json: /createdAt, matches: "^\\d{4}-\\d{2}-\\d{2}T" }
```

### Targets

Each check has exactly one target:

| Target key | Checks |
| --- | --- |
| `status: 200` | The HTTP status equals the number. Takes no other operator. |
| `json: /pointer` | A value in the JSON response body. `json: ""` is the whole document. |
| `header: Name` | A response header (case-insensitive name, first value). |
| `body: true` | The raw response text. |
| `var: name` | A variable (for example a captured value); `json:` then points inside it. |
| `responseTimeLtMs: 500` | The response arrived in under 500 ms. A target of its own. |

### Operators

A check holds one or more operators; all of them must pass.

| Operator | Meaning |
| --- | --- |
| `exists: true/false` | The location exists / does not exist. |
| `absent: true` | The location does not exist. |
| `type: string` | JSON type: `string`, `number`, `boolean`, `array`, `object` or `null`. |
| `eq: value` | Equals the value (templates allowed). Numbers compare by value. |
| `ne: value` | Does not equal the value. |
| `eqJson: /pointer` | Equals the value at another location in the same response. |
| `neJson: /pointer` | Differs from the value at another location in the same response. |
| `matches: regex` | The string value matches the regular expression (Go RE2 syntax, unanchored). |
| `min: n` / `max: n` | A number is at least / at most `n`. |
| `length: n` / `lengthMin: n` | An array, string or object has exactly / at least `n` elements. |
| `empty: true/false` | The array, string or object is empty / not empty. |
| `contains: value` | A string contains the text, or an array contains the value. |
| `any: { ... }` | At least one array element satisfies the nested check; its `json:` pointer is relative to the element. |
| `decodeJson: /pointer` | The value is base64 text of a JSON document; the other operators apply to the location inside it. |

A missing location fails an operator that needs a value (other than `exists`/`absent`) with
"location does not exist in the response". A body that is not valid JSON makes JSON checks
`errored`.

Examples:

```yaml
- { json: /status, eq: active }
- { json: /count, min: 1, max: 100 }
- { json: /items, type: array, empty: false }
- { json: /items, any: { json: /id, eq: "{{itemId}}" } }      # some element has this id
- { json: /page, neJson: /nextPage }
- { header: Content-Type, matches: "^application/json" }
- { body: true, contains: "ok" }
- { var: itemId, matches: "^[0-9a-f-]{36}$" }
- { json: /payload, decodeJson: /user/id, eq: 42 }             # base64 JSON inside a field
- { responseTimeLtMs: 800 }
```

### Testing error responses

An HTTP 4xx or 5xx response is **not** an error by itself; only your assertions decide the result.
To test a failure case, assert the failure:

```yaml
- id: reject-empty-name
  service: api
  method: POST
  path: /items
  json: { name: "" }
  assert:
    - name: Request is rejected
      checks:
        - { status: 400 }
        - { json: /error, eq: "name must not be empty" }
```

## Dependencies, exports and matrices

### `dependsOn` and `exports`

Scenarios are independent by default. When one needs another's work, declare it:

```yaml
- id: orders.create
  exports: [orderId]            # offered to dependents once this scenario passes
  steps:
    - id: create
      service: api
      method: POST
      path: /orders
      json: { sku: ABC }
      capture:
        orderId: { json: /id }

- id: orders.ship
  dependsOn: [orders.create]
  steps:
    - id: ship
      service: api
      method: POST
      path: /orders/{{orderId}}/ship
```

* A scenario whose dependency did not pass is `blocked` and sends no requests.
* A selected scenario automatically pulls in its dependencies, even through filters; these are
  shown as "by dependency".
* Dependency cycles, unknown ids and exporting a name that is never defined are validation errors.
* A name can be exported by only one scenario.

### `matrix`

A matrix expands one scenario into several. Each entry needs a string `suffix`; the other keys
become variables for that copy. The expanded scenario id is `<id>.<suffix>`.

```yaml
- id: items.names
  matrix:
    - { suffix: short,  name: "a" }
    - { suffix: spaces, name: "two words" }
    - { suffix: long,   name: "{{$random_hex 64}}" }
  steps:
    - id: create
      service: api
      method: POST
      path: /items
      json: { name: "{{name}}" }
      assert:
        - name: Name is stored unchanged
          checks:
            - { status: 201 }
            - { json: /name, eq: "{{name}}" }
```

## Polling

For readiness checks and eventually consistent reads, a `GET` step can repeat until a condition
holds:

```yaml
- id: wait-until-processed
  service: api
  method: GET
  path: /jobs/{{jobId}}
  poll:
    attempts: 20          # 1 to 120
    intervalSeconds: 3    # at least 1
    until:
      - { json: /state, eq: done }
  assert:
    - name: Job finished
      checks:
        - { json: /state, eq: done }
```

The step's captures and assertions use the final response. If the condition is not reached after
the last attempt the step is `errored`. Polling is rejected on any method other than `GET`, so a
mutating call is never retried. Every attempt is recorded as its own call.

## Cleanup steps

`cleanup` steps run after the scenario's steps, always, even when a step failed. They are for
removing what the scenario created. A cleanup step that cannot run because the variable it needs was
never created (the creating step never ran) is skipped, not failed. A cleanup failure marks an
otherwise passed scenario as `errored`, but never hides an earlier failure.

## Command reference

```
ferret run      --tests SUITE.yaml --output DIR [options]
ferret validate --tests SUITE.yaml [--env-file FILE]...
ferret list     --tests SUITE.yaml [--tag T]... [--exclude-tag T]...
ferret docs     --tests SUITE.yaml [--readme FILE] [--check]
ferret report   --input results.json [--output report.html]
ferret version
```

The suite path can also be given in the `TESTS` environment variable; `--tests` takes precedence.

### `ferret run`

| Option | Meaning |
| --- | --- |
| `--tests FILE` | The suite file. |
| `--output DIR` | Where `results.json` and `junit.xml` are written (required). Files from a previous run in this folder are removed first, so a run that stops early can never leave stale results behind. |
| `--env-file FILE` | A dotenv file of input values. Repeatable; later files override earlier ones. |
| `--tag T` | Run only scenarios with this tag. Repeatable; several tags are a union. |
| `--exclude-tag T` | Skip scenarios with this tag. Repeatable. |
| `--scenario ID` | Run only this scenario. Repeatable. Unknown ids are an error, with suggestions. |
| `--rerun-failed FILE` | Run again only the scenarios that did not pass in a previous `results.json`. |
| `--debug` | Print the step-by-step log to the console (see below). |
| `--log-file FILE` | Write the same log to a file (no log file is written unless you ask). |
| `--save-files DIR` | Save documents found in responses (PDF, ZIP and similar) as real files under `DIR`. |

Examples:

```sh
ferret run --tests suite.yaml --env-file .env --output reports
ferret run --tests suite.yaml --env-file .env --tag smoke --output reports/smoke
ferret run --tests suite.yaml --env-file .env --exclude-tag slow --output reports
ferret run --tests suite.yaml --env-file .env --scenario items.create-and-read --debug --output reports/one
ferret run --tests suite.yaml --env-file .env --rerun-failed reports/results.json --output reports/rerun
```

Unknown tags are errors. Inputs are resolved before any request is sent; if anything is missing or
invalid the run stops with exit code `2` and sends nothing.

A run with any filter is reported as partial ("3 of 40 scenarios selected, not full coverage") and
`results.json` records `"complete": false`.

### Re-running only what did not pass

After a run with failures, `--rerun-failed` runs just those scenarios again, so you do not wait for
the whole suite after fixing one thing:

```sh
ferret run --tests suite.yaml --env-file .env --output reports
# some scenarios failed; fix the cause, then:
ferret run --tests suite.yaml --env-file .env --rerun-failed reports/results.json --output reports/rerun
```

* The scenarios chosen are every one that was `failed`, `errored` or `blocked` in the given
  `results.json`.
* Scenarios they depend on are added automatically (shown as "by dependency"), so a retried
  scenario still has what it needs.
* It combines with `--tag`, `--exclude-tag` and `--scenario`, which add to the selection.
* If nothing in the file failed, ferret stops with exit code `2` and a message instead of running
  an empty selection.
* The previous file is read before the output folder is cleaned, so `--output` may point at the same
  folder as the file you are rerunning from. A separate folder keeps the original results.
* The result is a partial run and is recorded as such (`"complete": false`).

### `--debug` log

The log is printed scenario by scenario as the run proceeds:

```
---items.create-and-read----
----step.create  POST /items
     call #1 POST https://api.example.com/items -> 201 (84 ms)
----step.read  GET /items/{{itemId}}
     call #2 GET https://api.example.com/items/9f2c -> 200 (31 ms)
----step.delete  DELETE /items/{{itemId}}  (cleanup)
     call #3 DELETE https://api.example.com/items/9f2c -> 204 (22 ms)
-----Assertions
  1  [create] Status code is 201  PASS
  2  [create] Response describes the new item  PASS
  3  [read] Status code is 200  PASS
  4  [read] The item can be read back  PASS
PASS    items.create-and-read                    3 steps, 4 assertions, 0.1s
```

For a step that did not pass, the log also shows the problem and the request and response bodies.
Everything passes through the redactor first.

### `ferret validate`

Loads the suite and every scenario file and checks them without contacting any service: syntax,
unknown keys, ids, tags, dependencies and cycles, undefined variables, malformed regular
expressions, fixture paths, and the env files. Exit code `0` if valid, `2` otherwise.

### `ferret list`

Shows which scenarios a selection would run, with their tags, step and assertion counts, and
which inputs are required or optional for that selection.

### `ferret report`

Renders a saved `results.json` into one self-contained HTML file. It needs no credentials, no suite
files and no network. `--output` defaults to `report.html` next to the input.

### `ferret docs`

Generates the reference part of a test set's README from the suite. See
[Generated test documentation](#generated-test-documentation).

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Every selected assertion passed. |
| `1` | At least one scenario did not pass (or `docs --check` found an outdated README). |
| `2` | Usage, configuration or validation problem. No requests were sent. |

## Results

`ferret run` writes two files to `--output`:

* **`results.json`**: the full, sanitized record of the run.
* **`junit.xml`**: the same outcome in JUnit format for CI systems.

`ferret report` adds **`report.html`**.

`results.json` (format `version: 1`) contains:

```json
{
  "version": 1,
  "tool": "ferret",
  "run": { "suite": "...", "startedAt": "...", "finishedAt": "...", "durationMs": 1234,
           "selection": { "complete": true, "availableScenarios": 40, "selectedScenarios": 40 } },
  "totals": { "assertions": {"total": 0, "passed": 0, "failed": 0, "errored": 0, "blocked": 0},
              "setupAssertions": { "...": 0 }, "scenarios": { "...": 0 }, "httpCalls": 0 },
  "scenarios": [ { "id": "...", "status": "passed", "steps": [ { "id": "...", "assertions": [ ... ] } ] } ],
  "calls": [ { "seq": 1, "scenario": "...", "step": "...", "method": "GET", "url": "...",
               "status": 200, "requestBody": {}, "responseBody": {}, "durationMs": 31 } ]
}
```

Details worth knowing:

* Totals are derived from the records, never kept separately.
* Setup, helper and cleanup checks are counted under `setupAssertions`, not `assertions`.
* A recorded body is a preview of at most 64 KiB; the rest is marked as truncated. Responses larger
  than 128 MiB are rejected.
* Long base64 values (documents, certificates, signatures) are replaced with short summaries such
  as `<file: PDF, 15023 bytes>` or `<certificate: CN=..., serial ..., sha256 ...>`, so bodies stay
  readable. Use `--save-files` to keep the real documents.
* Captured values are not stored, only their names.

## Secret redaction

Anything persisted or printed (results, JUnit, report, log, console) is redacted:

* every input declared `secret: true`, and every capture marked `secret: true`, is replaced with
  `[REDACTED]` wherever its value appears, including its base64 and URL-encoded forms (values of
  at least 6 characters; shorter ones are not redacted by value to avoid masking unrelated text),
* well-known sensitive headers (`Authorization`, `Cookie`, `Set-Cookie`, `Apikey`, `X-Api-Key`, ...)
  are masked regardless of value,
* well-known sensitive JSON fields (`password`, `secret`, `token`, `client_secret`, `private_key`,
  `access_token`, ...) are masked regardless of value.

A secret learned later in a run (for example a token captured from a response) is also removed from
calls recorded earlier, by a final sweep before results are written. Mark every credential
`secret: true`; redaction by name is a safety net, not a substitute.

## Using ferret in CI

A typical pipeline step builds or pulls ferret, validates the suite, runs it, and keeps the output:

```yaml
steps:
  - name: validate
    commands:
      - ferret validate --tests tests/suite.yaml --env-file tests/env/ci.env

  - name: test
    commands:
      - ferret run --tests tests/suite.yaml --env-file tests/env/ci.env --output reports
    environment:
      API_TOKEN:
        from_secret: api_token       # your CI system's secret mechanism

  - name: report
    when: { status: [success, failure] }
    commands:
      - ferret report --input reports/results.json --output reports/report.html
```

Put non-secret values (URLs) in a committed env file and inject secrets through environment
variables, which take priority over env files. Have the CI system publish `reports/junit.xml` as a
test report and `reports/report.html` as an artifact. Exit code `1` fails the job.

## Docker

```sh
docker build -t ferret .
docker build --build-arg VERSION=1.0.0 -t ferret:1.0.0 .
```

The image contains only the static binary (and a CA bundle for HTTPS), runs as a CLI with
`/work` as its working directory, and defaults to `--help`. Mount your test folder there:

```sh
docker run --rm -v "$PWD":/work -e API_TOKEN=... \
  ferret run --tests tests/suite.yaml --env-file tests/env/ci.env --output reports
```

## Generated test documentation

`ferret docs` keeps a human-readable list of your tests in sync with the YAML. Put two marker lines
in a README next to `suite.yaml`:

```
<!-- tests:begin (generated by `ferret docs`; do not edit) -->
<!-- tests:end -->
```

`ferret docs --tests suite.yaml` fills the region between them with an inputs table, a table of tags
with counts, and every scenario with its steps and assertion names. Run it with `--check` in CI to
fail when the README is out of date:

```sh
ferret docs --tests suite.yaml --check    # exit 1 if the README needs regenerating
```

## Behavior to know about

* **Redirects are not followed.** A `3xx` response is the result you assert on, so a mutating request
  is never silently replayed to another location.
* **A failed step blocks the rest of its scenario.** Later steps are reported as `blocked` rather
  than run against missing data. Cleanup still runs.
* **Timeouts** default to 60 seconds per request, set suite-wide with `timeoutSeconds` or per step.
  A timeout or connection error makes the step `errored`.
* **Setup runs once** before scenarios and its failure blocks everything.
* **Order is the suite order.** Scenarios run sequentially in the order the scenario files list
  them, one request at a time.
* **Interrupt** (Ctrl+C) stops cleanly: remaining scenarios are recorded as blocked, and results for
  what already ran are still written.
* **Content types** for JSON, form, multipart and text bodies are set automatically. Override with a
  step `headers` entry if needed.
* **Numbers** in JSON are compared by value, so `3` and `3.0` are equal.
* **Strict YAML.** Unknown keys are errors everywhere, in the suite, scenario files and inputs file.

## Building from source

```sh
go vet ./...
go test ./...
go build -trimpath -ldflags="-s -w -X main.Version=1.0.0" -o ferret ./cmd/ferret
```

The only third-party dependency is `gopkg.in/yaml.v3`.

## License

MIT. See [LICENSE](LICENSE).
