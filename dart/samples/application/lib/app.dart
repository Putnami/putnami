import 'dart:io';

/// The greeting GET / answers.
const greeting = 'Hello from Dart\n';

/// Serves the routes of the sample on [address] and [port] until the server
/// closes: GET / answers [greeting], HEAD / its status, and any other request
/// 404.
Future<HttpServer> serve(InternetAddress address, int port) async {
  final server = await HttpServer.bind(address, port);
  server.listen(handle);
  return server;
}

/// Answers one request.
Future<void> handle(HttpRequest request) async {
  final response = request.response;
  if (request.uri.path == '/' && (request.method == 'GET' || request.method == 'HEAD')) {
    response.headers.contentType = ContentType.text;
    response.write(greeting);
  } else {
    response.statusCode = HttpStatus.notFound;
  }
  await response.close();
}
