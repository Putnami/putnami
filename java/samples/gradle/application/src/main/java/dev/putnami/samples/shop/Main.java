package dev.putnami.samples.shop;

import com.sun.net.httpserver.HttpServer;
import dev.putnami.samples.pricing.Discount;
import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;

/** Serves the total of a sample cart on GET / at the port PORT names. */
public final class Main {
    private Main() {}

    public static void main(String[] args) throws IOException {
        Checkout checkout = new Checkout(new Discount(10));
        if (args.length > 0) {
            long[] cents = new long[args.length];
            for (int i = 0; i < args.length; i++) {
                cents[i] = Long.parseLong(args[i]);
            }
            System.out.println(checkout.total(cents));
            return;
        }
        int port = Integer.parseInt(System.getenv().getOrDefault("PORT", "8080"));
        HttpServer server = HttpServer.create(new InetSocketAddress(port), 0);
        server.createContext("/", exchange -> {
            byte[] body = (checkout.total(1000, 500) + "\n").getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().set("Content-Type", "text/plain");
            if ("HEAD".equals(exchange.getRequestMethod())) {
                exchange.sendResponseHeaders(200, -1);
            } else {
                exchange.sendResponseHeaders(200, body.length);
                try (OutputStream out = exchange.getResponseBody()) {
                    out.write(body);
                }
            }
            exchange.close();
        });
        server.start();
        Runtime.getRuntime().addShutdownHook(new Thread(() -> server.stop(0)));
    }
}
