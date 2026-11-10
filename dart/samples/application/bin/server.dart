import 'dart:async';
import 'dart:io';

import 'package:dart_example_application/app.dart';

/// Serves the routes on 0.0.0.0 and the port PORT names, 8080 without one,
/// until SIGINT or SIGTERM.
Future<void> main() async {
  final port = int.tryParse(Platform.environment['PORT'] ?? '') ?? 8080;
  final server = await serve(InternetAddress.anyIPv4, port);
  stdout.writeln('listening on 0.0.0.0:${server.port}');
  final stop = Completer<void>();
  for (final signal in [ProcessSignal.sigint, ProcessSignal.sigterm]) {
    signal.watch().listen((_) {
      if (!stop.isCompleted) stop.complete();
    });
  }
  await stop.future;
  await server.close(force: true);
  exit(0);
}
