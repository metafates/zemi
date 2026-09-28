local test = zemi.test
local stdout = "main\n"
test.stub("lua.job.start", "git rev-parse *", function() return { stdout = stdout } end)

test("eq on a string", function()
	local body = zemi.json.encode({ current_condition = { { temp_C = "21", weatherDesc = { { value = "Sunny" } } } } })
	test.stub("lua.http.get", "https://wttr.in/*", { body = body })
	print("asking wttr.in")
	test.eq(test.tool("weather_now", { city = "Oslo" }), "Oslo: 21C, sunny")
end)

test("eq on a table", function() test.eq(test.render("demo.branch"), { "fix-login" }) end)

test("a call no stub answers", function() test.eq(test.tool("weather_now", { city = "Oslo" }), "Oslo: 21C, Sunny") end)

test("a callback that fails", function()
	stdout = ""
	test.advance(30000)
end)

test("a render that fails", function()
	local ctx = { usage = {} }
	test.render("demo.cost", ctx)
end)

test("a case that never settles", function()
	local function again() zemi.timer.after(0, again) end
	again()
	test.advance(1)
end)

test("a reply function with a misspelled field", function()
	test.stub("lua.http.get", "https://wttr.in/*", function() return { bdy = "{}" } end)
	test.eq(test.tool("weather_now", { city = "Oslo" }), "Oslo: 21C, Sunny")
end)

test("eq on an error naming a file", function()
	local _, err = pcall(zemi.http.get, "https://example.com/")
	pcall(zemi.http.get, "https://example.com/")
	test.eq(err, "permission ask required")
end)
