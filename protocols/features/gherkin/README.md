# Gherkin keyword catalogs

`locales/<language>.json` holds the structural keywords of one Gherkin language:
`feature`, `rule`, `background`, `scenario`, `scenarioOutline` and `examples`.
The Gherkin reader embeds them to read a file's `# language:` header.

They come unchanged from `gherkin-languages.json` of
[cucumber/gherkin](https://github.com/cucumber/gherkin) v42.0.1 (MIT). The step
keywords are left out, because the reader does not read steps. A new upstream
language is a new file here.
