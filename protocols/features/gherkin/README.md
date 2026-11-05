# Gherkin keyword catalogs

`locales/<language>.json` holds the keywords of one Gherkin language: `feature`,
`rule`, `background`, `scenario`, `scenarioOutline`, `examples` and the step
keywords `given`, `when`, `then`, `and` and `but`. The Gherkin reader embeds them
to read a file's `# language:` header. It reads the step keywords only to know
where a doc string may open.

They come unchanged from `gherkin-languages.json` of
[cucumber/gherkin](https://github.com/cucumber/gherkin) v42.0.1 (MIT), without
the `name` and `native` members. A new upstream language is a new file here.
