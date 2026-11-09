using Putnami.Example.Library;

var builder = WebApplication.CreateBuilder(args);
var app = builder.Build();

app.MapGet("/", () => Results.Json(new { total = Discount.Apply(10000, 15) }));

app.Run();
