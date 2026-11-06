using Calc;
using Microsoft.VisualStudio.TestTools.UnitTesting;

namespace Calc.Tests;

[TestClass]
public class CalculatorTests
{
    [TestMethod]
    public void Add() => Assert.AreEqual(5, Calculator.Add(2, 3));

    [TestMethod]
    [DataRow(4)]
    public void SignOfANonNegativeInput(int n) => Assert.AreEqual("positive", Calculator.Sign(n));

    [TestMethod]
    public void AddWrongOnPurpose() => Assert.AreEqual(5, Calculator.Add(2, 2));

    [TestMethod]
    [Ignore("not ready")]
    public void Skipped() { }
}
