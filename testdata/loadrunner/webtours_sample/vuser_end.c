vuser_end()
{
	web_url("home",
		"URL=http://127.0.0.1:8080/",
		"Resource=0",
		"Snapshot=t9.inf",
		"Mode=HTML",
		LAST);

	web_remove_auto_header("X-Client", "ImplicitGen=Yes", LAST);

	return 0;
}
