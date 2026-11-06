using Calc;
using Xunit;

namespace Calc.Tests;

public class CalculatorTests
{
    [Fact]
    public void Add() => Assert.Equal(5, Calculator.Add(2, 3));

    [Theory]
    [InlineData(4)]
    public void SignOfANonNegativeInput(int n) => Assert.Equal("positive", Calculator.Sign(n));

    [Fact]
    public void AddWrongOnPurpose() => Assert.Equal(5, Calculator.Add(2, 2));

    [Fact(Skip = "not ready")]
    public void Skipped() { }
}
