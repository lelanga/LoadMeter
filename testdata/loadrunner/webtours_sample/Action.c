/*
 * Hand-written fixture modelled on a generic login -> search -> order flow.
 * Targets the Load Meter mock server (internal/mockserver) on 127.0.0.1:8080.
 */
Action()
{
	int i;

	// 1. Login page: correlate the hidden anti-forgery token.
	web_reg_save_param_ex(
		"ParamName=FormToken",
		"LB=name=\"__RequestVerificationToken\" value=\"",
		"RB=\"",
		"Ordinal=1",
		SEARCH_FILTERS,
		"Scope=Body",
		LAST);

	web_reg_find("Text=<form", LAST);

	lr_start_transaction("01_open_login");

	web_url("login-page",
		"URL=http://127.0.0.1:8080/login-page",
		"Resource=0",
		"RecContentType=text/html",
		"Referer=",
		"Snapshot=t1.inf",
		"Mode=HTML",
		EXTRARES,
		"Url=/static/app.js", ENDITEM,
		LAST);

	lr_end_transaction("01_open_login", LR_AUTO);

	lr_think_time(2);

	// 2. Login: JSON response carries the API token and CSRF token.
	web_reg_save_param_json(
		"ParamName=CsrfToken",
		"QueryString=$.csrf",
		SEARCH_FILTERS,
		"Scope=Body",
		LAST);

	web_reg_save_param_regexp(
		"ParamName=AuthToken",
		"RegExp=\"token\":\"([0-9a-f]+)\"",
		"Ordinal=1",
		SEARCH_FILTERS,
		"Scope=Body",
		LAST);

	web_reg_find("Text=invalid credentials", "Fail=Found", LAST);

	lr_start_transaction("02_login");

	web_submit_data("login",
		"Action=http://127.0.0.1:8080/login",
		"Method=POST",
		"RecContentType=application/json",
		"Referer=http://127.0.0.1:8080/login-page",
		"Snapshot=t2.inf",
		"Mode=HTML",
		ITEMDATA,
		"Name=__RequestVerificationToken", "Value={FormToken}", ENDITEM,
		"Name=username", "Value={UserName}", ENDITEM,
		"Name=password", "Value={Password}", ENDITEM,
		LAST);

	lr_end_transaction("02_login", LR_AUTO);

	web_reg_find("Text=Welcome {UserName}", LAST);

	web_url("dashboard",
		"URL=http://127.0.0.1:8080/dashboard",
		"Resource=0",
		"Snapshot=t3.inf",
		"Mode=HTML",
		LAST);

	lr_think_time(3);

	// 3. Search the catalogue.
	lr_start_transaction("03_search");

	web_reg_save_param("FirstSku",
		"LB=\"id\":\"",
		"RB=\"",
		"Ord=1",
		"NotFound=warning",
		LAST);

	web_url("items",
		"URL=http://127.0.0.1:8080/api/items?q=widget&run={RunStamp}",
		"Resource=0",
		"RecContentType=application/json",
		"Snapshot=t4.inf",
		"Mode=HTTP",
		LAST);

	lr_end_transaction("03_search", LR_AUTO);

	/* Custom C loop: the iteration count is computed at run time. */
	for (i = 0; i < atoi(lr_eval_string("{OrderQty}")); i++) {
		lr_param_sprintf("PollPage", "%d", i + 1);
		web_url("items-poll",
			"URL=http://127.0.0.1:8080/api/items?page={PollPage}",
			LAST);
	}

	// 4. Place an order (needs the session cookie and the CSRF header).
	lr_start_transaction("04_order");

	web_add_header("X-CSRF-Token", "{CsrfToken}");

	web_reg_save_param_json(
		"ParamName=OrderId",
		"QueryString=$.orderId",
		SEARCH_FILTERS,
		"Scope=Body",
		LAST);

	web_reg_find("Text=\"status\":\"created\"", LAST);

	web_custom_request("create-order",
		"URL=http://127.0.0.1:8080/api/orders",
		"Method=POST",
		"Resource=0",
		"RecContentType=application/json",
		"EncType=application/json",
		"Snapshot=t5.inf",
		"Mode=HTTP",
		"Body={\"item\":\"{FirstSku}\","
		"\"qty\":{OrderQty}}",
		LAST);

	lr_end_transaction("04_order", LR_AUTO);

	lr_save_string(lr_eval_string("{OrderId}"), "LastOrder");

	web_url("order-status",
		"URL=http://127.0.0.1:8080/api/orders/{LastOrder}",
		"Resource=0",
		"Snapshot=t6.inf",
		"Mode=HTTP",
		LAST);

	/* A Windows Sockets call left over from a multi-protocol recording. */
	lrs_send("socket0", "buf0", LrsLastArg);

	lr_think_time(1);

	return 0;
}
