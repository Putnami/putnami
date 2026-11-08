package dev.putnami.samples.shop;

import dev.putnami.samples.pricing.Discount;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

/** Serves the total of a sample cart on GET / at the port SERVER_PORT names. */
@SpringBootApplication
@RestController
public class Application {
    private final Checkout checkout = new Checkout(new Discount(10));

    @GetMapping(value = "/", produces = "text/plain")
    public String total() {
        return checkout.total(1000, 500) + "\n";
    }

    public static void main(String[] args) {
        SpringApplication.run(Application.class, args);
    }
}
