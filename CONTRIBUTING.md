# Contributing

Thanks for helping improve BB Graph Assistant. The project is in alpha, so focused bug reports and small, reviewable pull requests are especially useful.

## Before opening a pull request

- Describe the user-facing behavior or defect and the evidence for it.
- Keep changes scoped and update the relevant documentation.
- Use synthetic fixtures or material with a clear redistribution license.
- Do not include raw proxy traffic, capture databases, cookies, credentials, API tokens, private hostnames, or personal account data.
- Report vulnerabilities privately through GitHub's private vulnerability reporting feature when it is available for this repository. Do not open a public issue containing an exploitable detail.

## Local checks

Run the Go checks:

```sh
go test ./...
go vet ./...
```

Run the Python checks:

```sh
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -e '.[kb,mcp,test]'
python -m pytest
```

Pull requests should explain which checks ran and note any that could not run. Do not claim Caido compatibility or complete coverage based only on fixture tests.
