using Calc;
using NUnit.Framework;

namespace Calc.Tests;

public class CalculatorTests
{
    [Test]
    public void Add() => Assert.That(Calculator.Add(2, 3), Is.EqualTo(5));

    [TestCase(4)]
    public void SignOfANonNegativeInput(int n) => Assert.That(Calculator.Sign(n), Is.EqualTo("positive"));

    [Test]
    public void AddWrongOnPurpose() => Assert.That(Calculator.Add(2, 2), Is.EqualTo(5));

    [Test]
    [Ignore("not ready")]
    public void Skipped() { }
}
