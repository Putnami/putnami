# @putnami/php sample

Two Composer components: `application` requires `library` through a path
repository. Each has its own `vendor/`. `application` serves
`public/index.php`.

These projects carry the `e2e` tag, so the default gate skips them. To run
them, you need neither PHP nor Composer: `putnami install` pins PHP, and the
first task runs `composer install`. Coverage needs a PHP with PCOV or Xdebug on
`PATH`; without one, `test` warns and writes no Clover report.

```bash
putnami install
putnami projects sync
putnami lint,test,build --tag e2e --projects /php/samples/library,/php/samples/application
putnami serve --projects /php/samples/application --port 3456
putnami qualify /php/samples/application --target local
putnami package --docker --docker-load --projects /php/samples/application
```

`application/schema/http-routes.json` lists the routes `putnami qualify` requests.
