# @putnami/php sample

Two Composer components: `application` requires `library` through a path
repository. Each has its own `vendor/`.

These projects carry the `e2e` tag, so the default gate skips them. To run
them, you need PHP 8.3 or later, Composer, and PCOV or Xdebug:

```bash
(cd php/samples/library && composer install)
(cd php/samples/application && composer install)
putnami projects sync
putnami lint,test,build --tag e2e --projects /php/samples/library,/php/samples/application
```
