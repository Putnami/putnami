import 'dart:convert';
import 'dart:io';

import 'package:dart_example_application/app.dart';
import 'package:test/test.dart';

void main() {
  late HttpServer server;
  late HttpClient client;

  setUp(() async {
    server = await serve(InternetAddress.loopbackIPv4, 0);
    client = HttpClient();
  });

  tearDown(() async {
    client.close(force: true);
    await server.close(force: true);
  });

  Future<HttpClientResponse> send(String method, String path) async {
    final request = await client.open(method, 'localhost', server.port, path);
    return request.close();
  }

  test('GET / answers the greeting', () async {
    final response = await send('GET', '/');
    expect(response.statusCode, HttpStatus.ok);
    expect(await response.transform(utf8.decoder).join(), greeting);
  });

  test('HEAD / answers its status and no body', () async {
    final response = await send('HEAD', '/');
    expect(response.statusCode, HttpStatus.ok);
    expect(await response.transform(utf8.decoder).join(), isEmpty);
  });

  test('another path answers 404', () async {
    final response = await send('GET', '/missing');
    expect(response.statusCode, HttpStatus.notFound);
  });
}
